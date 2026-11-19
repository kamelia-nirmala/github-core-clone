// Git object helpers: commit/tree parsing and serialization.
// Ported from store.ts (parseTree/buildTree) and rest-contents.ts
// (takeApartCommit) — same bytes, same rules.
package main

import (
	"bytes"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
)

// formatTime renders an epoch second as the forge's ISO timestamp
// ("2025-01-01T00:00:01.000Z"). Same layout as Forge.NowIso.
func formatTime(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// CommitMeta is a commit object taken apart.
type CommitMeta struct {
	Tree    string
	Parents []string
	Message string
	Epoch   int64 // author timestamp, seconds since epoch
}

// parseCommit splits a raw commit object at the first blank line; a message
// ending in one newline loses that newline; no blank line means no message.
// The author line carries the timestamp as its second-to-last word.
func parseCommit(data []byte) CommitMeta {
	text := string(data)
	splitAt := strings.Index(text, "\n\n")
	header := text
	message := ""
	if splitAt >= 0 {
		header = text[:splitAt]
		message = text[splitAt+2:]
	}
	if strings.HasSuffix(message, "\n") {
		message = message[:len(message)-1]
	}
	meta := CommitMeta{Parents: []string{}, Message: message}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			meta.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			meta.Parents = append(meta.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			words := strings.Fields(strings.TrimSpace(line))
			if len(words) >= 2 {
				stampWord := strings.ReplaceAll(strings.ReplaceAll(words[len(words)-2], "<", ""), ">", "")
				if isDigits(stampWord) {
					if n, err := strconv.ParseInt(stampWord, 10, 64); err == nil {
						meta.Epoch = n
					}
				}
			}
		}
	}
	return meta
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// namedTreeEntry is one entry of a tree object (mode, name, oid).
type namedTreeEntry struct {
	Mode string
	Name string
	OID  string
}

// parseTree reads a tree object's raw bytes into entries.
func parseTree(data []byte) []namedTreeEntry {
	entries := []namedTreeEntry{}
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], ' ')
		if space < 0 {
			break
		}
		space += off
		mode := string(data[off:space])
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			break
		}
		nul += space + 1
		name := string(data[space+1 : nul])
		if nul+21 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, namedTreeEntry{Mode: mode, Name: name, OID: oid})
		off = nul + 21
	}
	return entries
}

// serializeTree builds a tree object from entries, sorted in git order
// (directories sort as if named "name/").
func serializeTree(entries []namedTreeEntry) []byte {
	sorted := make([]namedTreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ai, bj := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ai += "/"
		}
		if sorted[j].Mode == "40000" {
			bj += "/"
		}
		return ai < bj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		raw, err := hex.DecodeString(e.OID)
		if err == nil {
			buf.Write(raw)
		}
	}
	return buf.Bytes()
}
