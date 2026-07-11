package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// jsonNode is a small source index. Unlike searching for a repeated key name,
// it retains the offset of each key at its exact object/array path.
type jsonField struct {
	keyStart int
	value    *jsonNode
}

type jsonNode struct {
	start  int
	end    int
	fields map[string]jsonField
	elems  []*jsonNode
}

type jsonCursor struct {
	data []byte
	pos  int
}

func parseJSONNode(data []byte) (*jsonNode, error) {
	c := &jsonCursor{data: data}
	return c.value()
}

func (c *jsonCursor) skip() {
	for c.pos < len(c.data) && strings.ContainsRune(" \n\r\t", rune(c.data[c.pos])) {
		c.pos++
	}
}

func (c *jsonCursor) value() (*jsonNode, error) {
	c.skip()
	if c.pos >= len(c.data) {
		return nil, io.ErrUnexpectedEOF
	}
	n := &jsonNode{start: c.pos}
	switch c.data[c.pos] {
	case '{':
		n.fields = map[string]jsonField{}
		c.pos++
		c.skip()
		for c.pos < len(c.data) && c.data[c.pos] != '}' {
			keyStart := c.pos
			key, err := c.stringValue()
			if err != nil {
				return nil, err
			}
			c.skip()
			if c.pos >= len(c.data) || c.data[c.pos] != ':' {
				return nil, errors.New("expected colon")
			}
			c.pos++
			value, err := c.value()
			if err != nil {
				return nil, err
			}
			n.fields[key] = jsonField{keyStart: keyStart, value: value}
			c.skip()
			if c.pos < len(c.data) && c.data[c.pos] == ',' {
				c.pos++
				c.skip()
			}
		}
		if c.pos >= len(c.data) {
			return nil, io.ErrUnexpectedEOF
		}
		c.pos++
	case '[':
		c.pos++
		c.skip()
		for c.pos < len(c.data) && c.data[c.pos] != ']' {
			value, err := c.value()
			if err != nil {
				return nil, err
			}
			n.elems = append(n.elems, value)
			c.skip()
			if c.pos < len(c.data) && c.data[c.pos] == ',' {
				c.pos++
				c.skip()
			}
		}
		if c.pos >= len(c.data) {
			return nil, io.ErrUnexpectedEOF
		}
		c.pos++
	case '"':
		if _, err := c.stringValue(); err != nil {
			return nil, err
		}
	default:
		for c.pos < len(c.data) && !strings.ContainsRune(" \n\r\t,]}", rune(c.data[c.pos])) {
			c.pos++
		}
	}
	n.end = c.pos
	return n, nil
}

func (c *jsonCursor) stringValue() (string, error) {
	if c.pos >= len(c.data) || c.data[c.pos] != '"' {
		return "", errors.New("expected string")
	}
	start := c.pos
	c.pos++
	escaped := false
	for c.pos < len(c.data) {
		b := c.data[c.pos]
		c.pos++
		if escaped {
			escaped = false
			continue
		}
		if b == '\\' {
			escaped = true
			continue
		}
		if b == '"' {
			var s string
			if err := json.Unmarshal(c.data[start:c.pos], &s); err != nil {
				return "", err
			}
			return s, nil
		}
	}
	return "", io.ErrUnexpectedEOF
}

func (n *jsonNode) pathOffset(path string) (int, bool) {
	if path == "$" {
		return n.start, true
	}
	if !strings.HasPrefix(path, "$") {
		return 0, false
	}
	cur, offset := n, n.start
	for i := 1; i < len(path); {
		switch path[i] {
		case '.':
			i++
			start := i
			for i < len(path) && path[i] != '.' && path[i] != '[' {
				i++
			}
			field, ok := cur.fields[path[start:i]]
			if !ok {
				return 0, false
			}
			offset, cur = field.keyStart, field.value
		case '[':
			i++
			start := i
			for i < len(path) && path[i] != ']' {
				i++
			}
			if i >= len(path) {
				return 0, false
			}
			var index int
			if _, err := fmt.Sscanf(path[start:i], "%d", &index); err != nil || index < 0 || index >= len(cur.elems) {
				return 0, false
			}
			cur, offset = cur.elems[index], cur.elems[index].start
			i++
		default:
			return 0, false
		}
	}
	return offset, true
}

func (n *jsonNode) pathAt(offset int, path string) string {
	for name, field := range n.fields {
		if offset >= field.value.start && offset <= field.value.end {
			return field.value.pathAt(offset, path+"."+name)
		}
	}
	for i, elem := range n.elems {
		if offset >= elem.start && offset <= elem.end {
			return elem.pathAt(offset, fmt.Sprintf("%s[%d]", path, i))
		}
	}
	return path
}
