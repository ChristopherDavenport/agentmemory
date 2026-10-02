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

// headLine returns the journal's first line, up to the first newline
// and at most headLimit bytes, which identifies the file a cursor's
// offsets belong to. A journal replaced by another of the same length
// or longer would otherwise be resumed at an offset that is not where
// a record starts.
func (s *Store) headLine() ([]byte, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("filestore: open journal: %w", err)
	}
	defer f.Close()
	line, err := bufio.NewReader(io.LimitReader(f, headLimit)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("filestore: read journal: %w", err)
	}
	return line, nil
}

// headLimit bounds the first line a cursor is identified by, so a
// journal whose first record is enormous is read in part rather than
// whole.
const headLimit = 64 << 10

// lastSeq returns the Seq of the journal's last record, or 0 for a
// journal with none. It reads the file's tail rather than the whole
// file, since the journal is unbounded and a write needs one number
// from it. A trailing partial line is a write that was cut off and a
// line that is not a record is damage; neither carries a number, so
// the read walks back past them to the last record. A write that
// refused on a damaged tail would refuse for ever, since nothing
// repairs the journal.
func (s *Store) lastSeq() (uint64, error) {
	f, err := os.Open(s.journalPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("filestore: open journal: %w", err)
	}
	defer f.Close()
	var seq uint64
	err = tailLines(f, func(line []byte) bool {
		var c agentmemory.Change
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &c) != nil {
			return true
		}
		seq = c.Seq
		return false
	})
	if err != nil {
		return 0, fmt.Errorf("filestore: read journal tail: %w", err)
	}
	return seq, nil
}

// tailLines calls fn with each complete line of f, without its
// newline, from the last to the first, until fn returns false. A
// trailing partial line, from a write that was cut off, is not a line.
// It reads backwards in growing chunks, so a caller that stops at the
// last line reads the file's tail and not the file.
func tailLines(f *os.File, fn func(line []byte) bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	start := info.Size()
	chunk := int64(4 << 10)
	var buf []byte // the file from start to the end of the last line kept
	limit := -1    // index in buf of the newline ending the next line to yield
	for start > 0 {
		n := min(chunk, start)
		more := make([]byte, int(n)+len(buf))
		if _, err := f.ReadAt(more[:n], start-n); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		copy(more[n:], buf)
		buf = more
		start -= n
		chunk *= 2
		if limit < 0 {
			// Drop the trailing partial line: lines end at the last
			// newline, and a file without one holds no line.
			limit = bytes.LastIndexByte(buf, '\n')
			if limit < 0 {
				continue
			}
		} else {
			limit += int(n)
		}
		for {
			begin := bytes.LastIndexByte(buf[:limit], '\n')
			if begin < 0 {
				if start == 0 {
					// The file's first line.
					fn(buf[:limit])
					return nil
				}
				break // its start is in bytes not read yet
			}
			if !fn(buf[begin+1 : limit]) {
				return nil
			}
			limit = begin
		}
		buf = buf[:limit+1]
	}
	return nil
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
