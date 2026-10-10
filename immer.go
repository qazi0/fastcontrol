package main

// Applies immer-style JSON patches ({op, path, value}) to a decoded JSON tree.
// The desktop app sends thread state changes in this format.

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type patch struct {
	Op    string            `json:"op"`
	Path  []json.RawMessage `json:"path"`
	Value json.RawMessage   `json:"value"`
}

// pathKey returns a path element as a string key and, for numeric elements, an int.
func pathKey(raw json.RawMessage) (string, int, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if n, err := strconv.Atoi(s); err == nil {
			return s, n, true
		}
		return s, -1, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.Itoa(n), n, true
	}
	return string(raw), -1, false
}

func decodeValue(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v any
	err := json.Unmarshal(raw, &v)
	return v, err
}

func applyPatch(root any, p patch) (any, error) {
	if len(p.Path) == 0 {
		if p.Op == "replace" || p.Op == "add" {
			return decodeValue(p.Value)
		}
		return nil, nil
	}
	return applyAt(root, p.Path, p)
}

func applyAt(node any, path []json.RawMessage, p patch) (any, error) {
	key, idx, numeric := pathKey(path[0])
	last := len(path) == 1
	switch n := node.(type) {
	case map[string]any:
		if last {
			switch p.Op {
			case "remove":
				delete(n, key)
			default:
				v, err := decodeValue(p.Value)
				if err != nil {
					return nil, err
				}
				n[key] = v
			}
			return n, nil
		}
		child, ok := n[key]
		if !ok {
			return nil, fmt.Errorf("missing key %q", key)
		}
		nc, err := applyAt(child, path[1:], p)
		if err != nil {
			return nil, err
		}
		n[key] = nc
		return n, nil
	case []any:
		if last && key == "length" {
			v, err := decodeValue(p.Value)
			if err != nil {
				return nil, err
			}
			f, _ := v.(float64)
			l := int(f)
			if l < len(n) {
				return n[:l], nil
			}
			for len(n) < l {
				n = append(n, nil)
			}
			return n, nil
		}
		if !numeric {
			if key == "-" && last && p.Op == "add" {
				v, err := decodeValue(p.Value)
				if err != nil {
					return nil, err
				}
				return append(n, v), nil
			}
			return nil, fmt.Errorf("non-numeric array index %q", key)
		}
		if last {
			switch p.Op {
			case "add":
				v, err := decodeValue(p.Value)
				if err != nil {
					return nil, err
				}
				if idx > len(n) {
					return nil, fmt.Errorf("add index %d beyond length %d", idx, len(n))
				}
				n = append(n, nil)
				copy(n[idx+1:], n[idx:])
				n[idx] = v
				return n, nil
			case "remove":
				if idx >= len(n) {
					return nil, fmt.Errorf("remove index %d beyond length %d", idx, len(n))
				}
				return append(n[:idx], n[idx+1:]...), nil
			default:
				v, err := decodeValue(p.Value)
				if err != nil {
					return nil, err
				}
				for idx >= len(n) {
					n = append(n, nil)
				}
				n[idx] = v
				return n, nil
			}
		}
		if idx >= len(n) {
			return nil, fmt.Errorf("index %d beyond length %d", idx, len(n))
		}
		nc, err := applyAt(n[idx], path[1:], p)
		if err != nil {
			return nil, err
		}
		n[idx] = nc
		return n, nil
	default:
		return nil, fmt.Errorf("cannot descend into %T at %q", node, key)
	}
}
