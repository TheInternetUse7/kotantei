package config

import (
	"bytes"
	"errors"
)

// normalizeJSONC replaces comments and trailing commas with whitespace. The
// ordinary JSON decoder still validates all values and the configuration schema.
func normalizeJSONC(data []byte) ([]byte, error) {
	text := append([]byte(nil), data...)
	for i := 0; i < len(text); i++ {
		if text[i] == '"' {
			i = jsonStringEnd(text, i)
			continue
		}
		if text[i] != '/' || i+1 == len(text) {
			continue
		}
		end := i + 2
		switch text[i+1] {
		case '/':
			for end < len(text) && text[end] != '\r' && text[end] != '\n' {
				end++
			}
		case '*':
			closing := bytes.Index(text[end:], []byte("*/"))
			if closing < 0 {
				return nil, errors.New("invalid configuration JSONC")
			}
			end += closing + 2
		default:
			continue
		}
		for ; i < end; i++ {
			if text[i] != '\r' && text[i] != '\n' {
				text[i] = ' '
			}
		}
		i--
	}
	for i := 0; i < len(text); i++ {
		if text[i] == '"' {
			i = jsonStringEnd(text, i)
			continue
		}
		if text[i] != ',' {
			continue
		}
		next := i + 1
		for next < len(text) && jsonSpace(text[next]) {
			next++
		}
		if next == len(text) || (text[next] != '}' && text[next] != ']') {
			continue
		}
		previous := i - 1
		for previous >= 0 && jsonSpace(text[previous]) {
			previous--
		}
		// A comma after an opener or a missing value is not a trailing comma.
		if previous < 0 || bytes.ContainsRune([]byte("[{,:"), rune(text[previous])) {
			continue
		}
		text[i] = ' '
	}
	return text, nil
}

func jsonStringEnd(text []byte, start int) int {
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return len(text)
}

func jsonSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}
