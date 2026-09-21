package filestore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ChristopherDavenport/agentmemory"
)

// journalName is the append-only record of every change, one JSON
// object per line in the shape of [agentmemory.Change], at the store's
// root so one sequence orders every scope.
const journalName = "journal.jsonl"

// appendJournal writes one record and returns the journal's size after
// it, which is where the next record starts. The caller holds the lock
// and has set Seq from [lastSeq].
func (s *Store) appendJournal(c agentmemory.Change) (int64, error) {
	line, err := json.Marshal(c)
	if err != nil {
		return 0, fmt.Errorf("filestore: encode journal record: %w", err)
	}
	f, err := os.OpenFile(s.journalPath(), os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return 0, fmt.Errorf("filestore: open journal: %w", err)
	}
	// A write that was cut off leaves a partial last line. Terminate
	// it first, so it stays one damaged line that readers report and
	// this record stays whole.
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		var tail [1]byte
		if _, err := f.ReadAt(tail[:], info.Size()-1); err == nil && tail[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	_, werr := f.Write(append(line, '\n'))
	if werr == nil {
		werr = f.Sync()
	}
	var end int64
	if werr == nil {
		if info, serr := f.Stat(); serr == nil {
			end = info.Size()
		} else {
			werr = serr
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return 0, fmt.Errorf("filestore: append journal: %w", werr)
	}
	return end, nil
}

// lastSeq returns the Seq of the journal's last record, or 0 for no
// journal. It reads the file's tail rather than the whole file, since
// the journal is unbounded and a write needs one number from it.
func (s *Store) lastSeq() (uint64, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("filestore: open journal: %w", err)
	}
	defer f.Close()
	line, err := lastLine(f)
	if err != nil {
		return 0, fmt.Errorf("filestore: read journal tail: %w", err)
	}
	if len(line) == 0 {
		return 0, nil
	}
	var probe struct {
		Seq uint64 `json:"seq"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return 0, fmt.Errorf("filestore: journal's last record is not a change: %w", err)
	}
	return probe.Seq, nil
}

// lastLine returns the last newline-terminated line of f, reading
// backwards in growing chunks. A trailing partial line, from a write
// that was cut off, is ignored.
func lastLine(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	chunk := int64(4 << 10)
	for {
		start := max(size-chunk, 0)
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		// Drop a trailing partial line, then find the line before it.
		end := bytes.LastIndexByte(buf, '\n')
		if end < 0 {
			if start == 0 {
				return nil, nil
			}
			chunk *= 2
			continue
		}
		begin := bytes.LastIndexByte(buf[:end], '\n')
		if begin < 0 && start > 0 {
			chunk *= 2
			continue
		}
		return buf[begin+1 : end], nil
	}
}

// readJournal calls fn with each record in order, starting after the
// given Seq, until fn returns false. A line that is not a record is
// reported through fn as an error, with a zero Change, and reading
// continues if fn returns true.
func (s *Store) readJournal(after uint64, fn func(agentmemory.Change, error) bool) {
	_, err := s.scanJournal(0, func(c agentmemory.Change, err error) bool {
		if err != nil || c.Seq > after {
			return fn(c, err)
		}
		return true
	})
	if err != nil {
		fn(agentmemory.Change{}, err)
	}
}

// scanJournal calls fn with each record from the byte offset off,
// which must be where a line starts, and returns the offset after the
// last complete line, so a reader that keeps it resumes there rather
// than reading the journal again. An offset past the end of the
// journal, from a file that was replaced or truncated, reads nothing
// and returns the size, which the caller compares with the offset it
// held. A line that is not a record is reported through fn as an
// error, with a zero Change, and reading continues if fn returns true.
func (s *Store) scanJournal(off int64, fn func(agentmemory.Change, error) bool) (int64, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return off, fmt.Errorf("filestore: open journal: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return off, fmt.Errorf("filestore: read journal: %w", err)
	}
	if off > info.Size() {
		return info.Size(), nil
	}
	if off > 0 {
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return off, fmt.Errorf("filestore: read journal: %w", err)
		}
	}
	end := off
	r := bufio.NewReader(f)
	for lineNo := 1; ; lineNo++ {
		line, err := r.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fn(agentmemory.Change{}, fmt.Errorf("filestore: read journal: %w", err))
			return end, nil
		}
		if len(line) > 0 && line[len(line)-1] == '\n' {
			end += int64(len(line))
			line = line[:len(line)-1]
		} else if errors.Is(err, io.EOF) {
			// A partial last line is a write that was cut off; the
			// record before it is the journal's last.
			return end, nil
		}
		if len(bytes.TrimSpace(line)) == 0 {
			if errors.Is(err, io.EOF) {
				return end, nil
			}
			continue
		}
		var c agentmemory.Change
		if uerr := json.Unmarshal(line, &c); uerr != nil {
			if !fn(agentmemory.Change{}, fmt.Errorf("filestore: journal line %d: %w", lineNo, uerr)) {
				return end, nil
			}
		} else if !fn(c, nil) {
			return end, nil
		}
		if errors.Is(err, io.EOF) {
			return end, nil
		}
	}
}
