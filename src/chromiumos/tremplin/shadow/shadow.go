// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

/*
The shadow package implements marshaling and unmarshaling of files in the
style of /etc/passwd.

Each of the files used by passwd-related tools (/etc/passwd, /etc/group,
/etc/shadow, and /etc/gshadow) are essentially the same format: one record per
line, with each record having its fields separated by colons. This package
makes some assumptions about the types of these fields to make manipulation
easier.

Valid record fields are string (user names, group names, gecos, homedir, etc),
uint32 (uid and gid), *uint64 (for empty integer fields, like inactive/expire),
and []string for comma-separated lists (group members).

Each record must have a nonempty string Name, which is the name of the user or
group.
*/
package shadow

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

// PasswdEntry encapsulates an entry in /etc/passwd. See passwd(5).
type PasswdEntry struct {
	Name     string
	Password string
	Uid      uint32
	Gid      uint32
	Gecos    string
	Homedir  string
	Shell    string
}

// ShadowEntry encapsulates an entry in /etc/shadow. See shadow(5).
type ShadowEntry struct {
	Name       string
	Password   string
	LastChange *uint64
	Min        *uint64
	Max        *uint64
	Warn       *uint64
	Inactive   *uint64
	Expire     *uint64
	Reserved   string
}

// GroupEntry encapsulates an entry in /etc/group. See group(5).
type GroupEntry struct {
	Name     string
	Password string
	Gid      uint32
	UserList []string
}

// GroupShadowEntry encapsulates an entry in /etc/gshadow. See gshadow(5).
type GroupShadowEntry struct {
	Name     string
	Password string
	Admins   []string
	Members  []string
}

// PasswdFile contains a list of PasswdEntry entries.
type PasswdFile struct {
	Entries []PasswdEntry
}

// ShadowFile contains a list of ShadowEntry entries.
type ShadowFile struct {
	Entries []ShadowEntry
}

// GroupFile contains a list of GroupEntry entries.
type GroupFile struct {
	Entries []GroupEntry
}

// GroupShadowFile contains a list of GroupShadowEntry entries.
type GroupShadowFile struct {
	Entries []GroupShadowEntry
}

// MarshalText marshals the PasswdFile to the on-disk format as specified by
// passwd(5).
func (f PasswdFile) MarshalText() (text []byte, err error) {
	b := &bytes.Buffer{}

	if err := marshal(b, &f.Entries); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// UnmarshalText unmarshals from the on-disk format specified in passwd(5).
func (f *PasswdFile) UnmarshalText(text []byte) error {
	if err := unmarshal(bytes.NewReader(text), &f.Entries); err != nil {
		return err
	}

	return nil
}

// MarshalText marshals the ShadowFile to the on-disk format as specified by
// shadow(5).
func (f ShadowFile) MarshalText() (text []byte, err error) {
	b := &bytes.Buffer{}

	if err := marshal(b, &f.Entries); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// UnmarshalText unmarshals from the on-disk format specified in shadow(5).
func (f *ShadowFile) UnmarshalText(text []byte) error {
	if err := unmarshal(bytes.NewReader(text), &f.Entries); err != nil {
		return err
	}

	return nil
}

// MarshalText marshals the GroupFile to the on-disk format as specified by
// group(5).
func (f GroupFile) MarshalText() (text []byte, err error) {
	b := &bytes.Buffer{}

	if err := marshal(b, &f.Entries); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// UnmarshalText unmarshals from the on-disk format specified in group(5).
func (f *GroupFile) UnmarshalText(text []byte) error {
	if err := unmarshal(bytes.NewReader(text), &f.Entries); err != nil {
		return err
	}

	return nil
}

// MarshalText marshals the GroupShadowFile to the on-disk format as specified
// by gshadow(5).
func (f GroupShadowFile) MarshalText() (text []byte, err error) {
	b := &bytes.Buffer{}

	if err := marshal(b, &f.Entries); err != nil {
		return nil, err
	}

	return b.Bytes(), nil
}

// UnmarshalText unmarshals from the on-disk format specified in gshadow(5).
func (f *GroupShadowFile) UnmarshalText(text []byte) error {
	if err := unmarshal(bytes.NewReader(text), &f.Entries); err != nil {
		return err
	}

	return nil
}

// NewUint64 returns a pointer to a uint64. This is useful for initializing
// *uint64 fields in structs.
func NewUint64(x uint64) *uint64 {
	return &x
}

// unmarshal unmarshals from reader to v. v must be a pointer to a slice of
// structs.
func unmarshal(reader io.Reader, v interface{}) error {
	value := reflect.ValueOf(v)

	// Verify that v is a pointer to a slice.
	if value.Kind() != reflect.Ptr || value.Type().Elem().Kind() != reflect.Slice {
		return errors.New("unmarshal target must be a pointer to a slice")
	}

	// sliceValue is the underlying slice that is pointed to. This will allow
	// appending to the slice later.
	sliceValue := value.Elem()

	// structType is the underlying struct that represents a line in the file.
	// If v is a pointer to a slice of PasswdEntry, then structType is PasswdEntry.
	structType := value.Type().Elem().Elem()

	if structType.Kind() != reflect.Struct {
		return errors.New("unmarshal slice must contain structs")
	}

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		fields := strings.Split(line, ":")
		// Ignore blank lines.
		if len(fields) == 1 && len(fields[0]) == 0 {
			continue
		}

		// The number of fields in the unmarshal target struct must match the
		// number of fields on the line.
		if len(fields) != structType.NumField() {
			return fmt.Errorf("expected %d fields, got %d: %q", structType.NumField(), len(fields), line)
		}

		// Instantiate a new *Entry for this line.
		linePtr := reflect.New(structType)
		lineValue := linePtr.Elem()

		// Fill in the fields for the *Entry.
		for i := 0; i < structType.NumField(); i++ {
			structField := structType.Field(i)

			if structField.Name == "Name" && len(fields[i]) == 0 {
				//lint:ignore ST1005 "Name" is a field name, keep it capitalized as it appears in the code
				return errors.New("Name field must be nonempty")
			}

			switch k := structField.Type.Kind(); k {
			case reflect.Uint32:
				u, err := strconv.ParseUint(fields[i], 10, 32)
				if err != nil {
					return fmt.Errorf("failed to parse uint32 field: %v", err)
				}
				lineValue.Field(i).SetUint(u)
			case reflect.String:
				lineValue.Field(i).SetString(fields[i])
			case reflect.Slice:
				if structField.Type.Elem().Kind() != reflect.String {
					return errors.New("slices must only have strings as their fields")
				}
				sliceFields := strings.Split(fields[i], ",")
				if len(sliceFields) > 1 {
					for _, v := range sliceFields {
						if len(v) == 0 {
							return errors.New("slice field must be nonempty")
						}
					}
				} else {
					// A slice of length 1 can be an empty string - remove it.
					if len(sliceFields[0]) == 0 {
						sliceFields = []string{}
					}
				}
				lineValue.Field(i).Set(reflect.ValueOf(sliceFields))
			case reflect.Ptr:
				if structField.Type.Elem().Kind() != reflect.Uint64 {
					return errors.New("only pointers to uint64 are allowed as fields")
				}
				if len(fields[i]) != 0 {
					u, err := strconv.ParseUint(fields[i], 10, 64)
					if err != nil {
						return fmt.Errorf("failed to parse uint64 field: %v", err)
					}
					lineValue.Field(i).Set(reflect.ValueOf(&u))
				} else {
					lineValue.Field(i).Set(reflect.Zero(structField.Type))
				}
			default:
				return fmt.Errorf("invalid field kind: %v", k)
			}
		}

		// Append this *Entry to the user-supplied slice.
		sliceValue.Set(reflect.Append(sliceValue, lineValue))

	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read lines from reader: %v", err)
	}

	return nil
}

// marshal marshals v to the given writer. v must be a pointer to a slice of
// structs, or simply a slice of structs.
func marshal(writer io.Writer, v interface{}) error {
	value := reflect.ValueOf(v)
	valueType := value.Type()

	var sliceValue reflect.Value

	// Grab sliceValue, which may be behind a pointer.
	switch valueType.Kind() {
	case reflect.Ptr:
		if valueType.Elem().Kind() != reflect.Slice {
			return errors.New("pointer to marshal must point to a slice")
		}
		sliceValue = value.Elem()
	case reflect.Slice:
		sliceValue = value
	default:
		return errors.New("marshal value must be a slice or pointer to a slice")
	}

	// structType is the underlying Entry type, e.g. PasswdEntry.
	structType := sliceValue.Type().Elem()
	if structType.Kind() != reflect.Struct {
		return errors.New("marshal slice must contain structs")
	}

	for i := 0; i < sliceValue.Len(); i++ {
		entryValue := sliceValue.Index(i)

		// Marshal the individual Entry fields.
		for j := 0; j < entryValue.NumField(); j++ {
			entryFieldValue := entryValue.Field(j)

			switch k := entryFieldValue.Type().Kind(); k {
			case reflect.Uint32:
				s := strconv.FormatUint(entryFieldValue.Uint(), 10)
				_, err := io.WriteString(writer, s)
				if err != nil {
					return fmt.Errorf("failed to output uint32 field: %v", err)
				}
			case reflect.String:
				_, err := io.WriteString(writer, entryFieldValue.String())
				if err != nil {
					return fmt.Errorf("failed to output string field: %v", err)
				}
			case reflect.Slice:
				for l := 0; l < entryFieldValue.Len(); l++ {
					_, err := io.WriteString(writer, entryFieldValue.Index(l).String())
					if err != nil {
						return fmt.Errorf("failed to output slice field: %v", err)
					}
					if l != (entryFieldValue.Len() - 1) {
						_, err = io.WriteString(writer, ",")
						if err != nil {
							return fmt.Errorf("failed to output slice string separator: %v", err)
						}
					}
				}
			case reflect.Ptr:
				if !entryFieldValue.IsNil() {
					if entryFieldValue.Elem().Kind() != reflect.Uint64 {
						return errors.New("only pointers to uint64 are allowed as fields")
					}

					s := strconv.FormatUint(entryFieldValue.Elem().Uint(), 10)
					_, err := io.WriteString(writer, s)
					if err != nil {
						return fmt.Errorf("failed to output *uint64 field: %v", err)
					}
				}
			default:
				return fmt.Errorf("invalid field kind: %v", k)
			}

			// Write a ":" field separator on all but the last field.
			if j != (entryValue.NumField() - 1) {
				_, err := io.WriteString(writer, ":")
				if err != nil {
					return fmt.Errorf("failed to write field separator: %v", err)
				}
			}
		}

		_, err := io.WriteString(writer, "\n")
		if err != nil {
			return fmt.Errorf("failed to write newline: %v", err)
		}
	}
	return nil
}
