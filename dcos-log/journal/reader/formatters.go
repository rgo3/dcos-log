package reader

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/coreos/go-systemd/sdjournal"
)

// ContentType is used in response header.
type ContentType string

var (
	// ContentTypePlainText is a ContentType header for plain text logs.
	ContentTypePlainText = "text/plain"

	// ContentTypeApplicationJSON is a ContentType header for json logs.
	ContentTypeApplicationJSON = "application/json"

	// ContentTypeEventStream is a ContentType header for event-stream logs.
	ContentTypeEventStream = "text/event-stream"
)

// EntryFormatter is an interface used by journal to write in a specific format.
type EntryFormatter interface {
	// GetContentType returns a content type for the entry formatter.
	GetContentType() string

	// FormatEntry accepts `sdjournal.JournalEntry` and returns an array of bytes.
	FormatEntry(*sdjournal.JournalEntry) ([]byte, error)
}

// FormatText implements EntryFormatter for text logs.
type FormatText struct{}

// GetContentType returns "text/plain"
func (j FormatText) GetContentType() string {
	return ContentTypePlainText
}

// FormatEntry formats sdjournal.JournalEntry to a text log line.
func (j FormatText) FormatEntry(entry *sdjournal.JournalEntry) (entryBytes []byte, err error) {
	// return empty if field MESSAGE not found
	message, ok := entry.Fields["MESSAGE"]
	if !ok {
		return entryBytes, nil
	}

	// text format: "date _HOSTNAME SYSLOG_IDENTIFIER[_PID]: MESSAGE
	// entry.RealtimeTimestamp returns a unix time in microseconds
	// https://www.freedesktop.org/software/systemd/man/sd_journal_get_realtime_usec.html
	l := logTextEntry{}
	t := time.Unix(int64(entry.RealtimeTimestamp)/1000000, 0)
	l.Add(t.Format(time.ANSIC))

	if hostname, ok := entry.Fields["_HOSTNAME"]; ok {
		l.Add(hostname)
	}

	if syslogID, ok := entry.Fields["SYSLOG_IDENTIFIER"]; ok {
		l.Add(syslogID)
	}

	if pid, ok := entry.Fields["_PID"]; ok {
		l.Add("[" + pid + "]")
	}

	l.Add(message)
	return l.ToBytes(), nil
}

// FormatJSON implements EntryFormatter for json logs.
type FormatJSON struct{}

// GetContentType returns "application/json"
func (j FormatJSON) GetContentType() string {
	return ContentTypeApplicationJSON
}

// FormatEntry formats sdjournal.JournalEntry to a json log entry.
func (j FormatJSON) FormatEntry(entry *sdjournal.JournalEntry) ([]byte, error) {
	entryBytes, err := marshalJournalEntry(entry)
	if err != nil {
		return entryBytes, err
	}

	entryPostfix := []byte("\n")
	return append(entryBytes, entryPostfix...), nil
}

// FormatSSE implements EntryFormatter for server sent event logs.
// Must be in the following format: data: {...}\n\n
type FormatSSE struct{}

// GetContentType returns "text/event-stream"
func (j FormatSSE) GetContentType() string {
	return ContentTypeEventStream
}

// FormatEntry formats sdjournal.JournalEntry to a server sent event log entry.
func (j FormatSSE) FormatEntry(entry *sdjournal.JournalEntry) ([]byte, error) {
	// Server sent events require \n\n at the end of the entry.
	entryBytes, err := marshalJournalEntry(entry)
	if err != nil {
		return entryBytes, err
	}

	entryPrefix := []byte("data: ")
	entryPostfix := []byte("\n\n")
	entryWithPostfix := append(entryBytes, entryPostfix...)
	entrySSE := append(entryPrefix, entryWithPostfix...)
	return entrySSE, nil
}

func marshalJournalEntry(entry *sdjournal.JournalEntry) ([]byte, error) {
	formattedEntry := struct {
		Fields             map[string]string `json:"fields"`
		Cursor             string            `json:"cursor"`
		MonotonicTimestamp uint64            `json:"monotonic_timestamp"`
		RealtimeTimestamp  uint64            `json:"realtime_timestamp"`
	}{
		Fields:             entry.Fields,
		Cursor:             entry.Cursor,
		MonotonicTimestamp: entry.MonotonicTimestamp,
		RealtimeTimestamp:  entry.RealtimeTimestamp,
	}

	return json.Marshal(formattedEntry)
}

// helper function to build a log line from array of strings.
type logTextEntry []string

func (l *logTextEntry) Add(s string) {
	*l = append(*l, s)
}

func (l *logTextEntry) String() string {
	return strings.Join(*l, " ") + "\n"
}

func (l *logTextEntry) ToBytes() []byte {
	return []byte(l.String())
}
