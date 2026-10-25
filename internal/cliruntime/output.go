package cliruntime

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
)

// Buffer formatted output before publishing it, including errors from CSV or
// tabwriter. Sparse, heterogeneous rows must not expand without a byte bound.
type responseBuffer struct {
	data  bytes.Buffer
	limit int64
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.limit-int64(b.data.Len()) {
		return 0, errors.New("formatted response exceeds the configured byte limit")
	}
	return b.data.Write(p)
}

func writeResponse(out io.Writer, data []byte, format string, limit int64) error {
	if !json.Valid(data) {
		return &ExitError{3, "response is not JSON; use --output raw"}
	}
	buffer := &responseBuffer{limit: limit}
	if format == "json" {
		if err := json.Indent(&buffer.data, data, "", "  "); err != nil {
			return &ExitError{3, "cannot format JSON response"}
		}
		if _, err := buffer.Write([]byte{'\n'}); err != nil {
			return &ExitError{3, "formatted response exceeds the configured byte limit"}
		}
	} else {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return &ExitError{3, "cannot decode JSON response"}
		}
		columns, rows := responseRows(value)
		if len(rows) == 0 {
			return nil
		}
		if err := writeRows(buffer, columns, rows, format, limit); err != nil {
			return &ExitError{3, "cannot format response within the configured byte limit"}
		}
	}
	if _, err := buffer.data.WriteTo(out); err != nil {
		return &ExitError{3, "cannot write response"}
	}
	return nil
}

// JSON objects are single records, arrays are records, and an items envelope
// projects its records. JSON output retains the entire envelope and metadata.
func responseRows(value any) ([]string, []map[string]any) {
	if object, ok := value.(map[string]any); ok {
		if items, ok := object["items"].([]any); ok {
			value = items
		}
	}
	items, ok := value.([]any)
	if !ok {
		items = []any{value}
	}
	rows := make([]map[string]any, 0, len(items))
	keys := map[string]bool{}
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok || len(row) == 0 {
			row = map[string]any{"value": item}
		}
		rows = append(rows, row)
		for key := range row {
			keys[key] = true
		}
	}
	columns := make([]string, 0, len(keys))
	for key := range keys {
		columns = append(columns, key)
	}
	slices.Sort(columns)
	return columns, rows
}

func responseCell(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	// Values originate from the JSON decoder, so marshaling cannot fail. UseNumber
	// preserves large integers and decimal precision, including in nested cells.
	data, _ := json.Marshal(value)
	return string(data)
}

func writeRows(out io.Writer, columns []string, rows []map[string]any, format string, limit int64) error {
	var write func([]string) error
	var flush func() error
	if format == "csv" {
		writer := csv.NewWriter(out)
		write = writer.Write
		flush = func() error { writer.Flush(); return writer.Error() }
	} else {
		writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		remaining := limit
		write = func(record []string) error {
			for index := range record {
				record[index] = escapeTableCell(record[index])
			}
			line := strings.Join(record, "\t")
			if int64(len(line))+1 > remaining {
				return errors.New("table exceeds the configured byte limit")
			}
			remaining -= int64(len(line)) + 1
			_, err := fmt.Fprintln(writer, line)
			return err
		}
		flush = writer.Flush
	}
	if err := write(slices.Clone(columns)); err != nil {
		return err
	}
	for _, row := range rows {
		record := make([]string, len(columns))
		for index, key := range columns {
			if value, exists := row[key]; exists {
				record[index] = responseCell(value)
			}
		}
		if err := write(record); err != nil {
			return err
		}
	}
	return flush()
}

// Keep server-controlled text within its cell and prevent terminal escapes.
func escapeTableCell(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			quoted := strconv.QuoteRune(r)
			out.WriteString(quoted[1 : len(quoted)-1])
		} else {
			out.WriteRune(r)
		}
	}
	return out.String()
}
