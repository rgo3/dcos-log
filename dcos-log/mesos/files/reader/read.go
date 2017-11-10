package reader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Sirupsen/logrus"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	chunkSize = 1 << 16
)

type response struct {
	Data   string `json:"data"`
	Offset int    `json:"offset"`
}

// ErrEmptyParam ...
var ErrEmptyParam = errors.New("args cannot be empty")

func notEmpty(args []string) error {
	if len(args) == 0 {
		return ErrEmptyParam
	}

	for _, arg := range args {
		if arg == "" {
			return ErrEmptyParam
		}
	}

	return nil
}

// NewLineReader is a ReadManager constructor.
func NewLineReader(client *http.Client, masterURL url.URL, agentID, frameworkID, executorID, containerID, taskPath, file string,
	format Formatter, opts ...Option) (*ReadManager, error) {

	// make sure the required parameters are set properly
	if err := notEmpty([]string{agentID, frameworkID, executorID, containerID}); err != nil {
		return nil, err
	}

	sandboxPath := fmt.Sprintf("/var/lib/mesos/slave/slaves/%s/frameworks/%s/executors/%s/runs/%s/", agentID, frameworkID, executorID, containerID)
	if taskPath != "" {
		sandboxPath += fmt.Sprintf("tasks/%s/", taskPath)
	}

	rm := &ReadManager{
		client: client,

		File:         file,
		readEndpoint: masterURL,
		sandboxPath:  sandboxPath,
		formatFn:     format,
	}

	for _, opt := range opts {
		if opt != nil {
			if err := opt(rm); err != nil {
				return nil, err
			}
		}
	}

	if rm.readDirection == BottomToTop {
		size, err := rm.fileLen(context.TODO())
		if err != nil {
			return nil, err
		}

		rm.offset = size
		foundLines := 0
		offset := size - chunkSize
		for {
			// if the offset is 0 or negative value, the means we reached the top of the file.
			// we can just set the offset to 0 and read the entire file
			if offset < 1 {
				rm.offset = 0
				break
			}

			lines, delta, err := rm.read(context.TODO(), offset, chunkSize, reverse)
			if err != nil {
				return nil, err
			}

			// if the required number of lines found, we need to calculate an offset
			if foundLines+len(lines) >= rm.n {
				diff := rm.n - foundLines
				for i := len(lines) - diff; i < len(lines); i++ {
					rm.offset -= len(lines[i].Message) + 1
				}
				break
			} else {
				// if the current chunk contains less then requested lines, then add to a counter
				// and continue search.
				foundLines += len(lines)
			}

			offset -= chunkSize - delta //+ 7
			rm.offset = offset
		}
	}

	return rm, nil
}

// ReadDirection ...
type ReadDirection int

const (
	// TopToBottom ...
	TopToBottom ReadDirection = iota

	// BottomToTop ...
	BottomToTop
)

// ReadManager ...
// http://mesos.apache.org/documentation/latest/endpoints/files/read/
type ReadManager struct {
	client       *http.Client
	readEndpoint url.URL
	sandboxPath  string
	header       http.Header

	readDirection ReadDirection
	n             int
	File          string

	size   int
	offset int
	lines  []Line

	readLines int
	stream    bool

	formatFn Formatter
}

func (rm *ReadManager) do(req *http.Request) (*response, error) {
	resp, err := rm.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status %d", resp.StatusCode)
	}

	data := &response{}
	if err := json.NewDecoder(resp.Body).Decode(data); err != nil {
		return nil, err
	}

	return data, nil
}

func (rm *ReadManager) fileLen(ctx context.Context) (int, error) {
	v := url.Values{}
	v.Add("path", rm.sandboxPath+rm.File)
	v.Add("offset", "-1")
	newURL := rm.readEndpoint
	newURL.RawQuery = v.Encode()

	// fmt.Println(newURL.String())
	req, err := http.NewRequest("GET", newURL.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header = rm.header

	resp, err := rm.do(req.WithContext(ctx))
	if err != nil {
		return 0, err
	}

	return resp.Offset, nil
}

// Modifier ...
type Modifier func(s string) string

func (rm *ReadManager) read(ctx context.Context, offset, length int, modifier Modifier) ([]Line, int, error) {
	v := url.Values{}
	v.Add("path", rm.sandboxPath+rm.File)
	v.Add("offset", strconv.Itoa(offset))
	v.Add("length", strconv.Itoa(length))

	if modifier == nil {
		modifier = func(s string) string { return s }
	}

	newURL := rm.readEndpoint
	newURL.RawQuery = v.Encode()

	logrus.Info(newURL.String())

	req, err := http.NewRequest("GET", newURL.String(), nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header = rm.header
	resp, err := rm.do(req.WithContext(ctx))
	if err != nil {
		return nil, 0, err
	}
	lines := strings.Split(modifier(resp.Data), "\n")

	delta := 0
	if len(lines) > 1 {
		delta = len(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
	}

	linesWithOffset := make([]Line, len(lines))
	// accumulates the position of the line + \n
	accumulator := 0
	for i := 0; i < len(lines); i++ {
		linesWithOffset[i] = Line{
			Message: lines[i],
			Offset:  offset + accumulator,
			Size:    len(lines[i]),
		}
		accumulator += len(lines[i]) + 1
	}

	return linesWithOffset, delta, nil
}

// Prepand ...
func (rm *ReadManager) Prepand(s Line) {
	old := rm.lines
	rm.lines = append([]Line{s}, old...)
}

// Pop ...
func (rm *ReadManager) Pop() *Line {
	old := *rm
	n := len(old.lines)
	if n == 0 {
		return nil
	}

	x := old.lines[n-1]
	rm.lines = old.lines[0 : n-1]
	return &x
}

// Read ...
func (rm *ReadManager) Read(b []byte) (int, error) {
	if !rm.stream && rm.readLines == rm.n {
		return 0, io.EOF
	}

	if len(rm.lines) == 0 {
		lines, delta, err := rm.read(context.TODO(), rm.offset, chunkSize, nil)
		if err != nil {
			return 0, err
		}

		if len(lines) > 1 {
			linesLen := 0
			for _, line := range lines {
				rm.Prepand(line)
				linesLen += len(line.Message) + 1
			}

			if linesLen < chunkSize {
				rm.offset = rm.offset + linesLen - 1
			} else {
				rm.offset = (rm.offset + chunkSize) - delta - 1
			}
		}
	}

	line := rm.Pop()
	if line == nil || line.Message == "" {
		return 0, io.EOF
	}

	rm.readLines++
	return strings.NewReader(rm.formatFn(*line)).Read(b)
}

func reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
