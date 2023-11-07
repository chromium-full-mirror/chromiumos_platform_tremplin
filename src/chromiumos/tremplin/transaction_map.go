// Copyright 2019 The ChromiumOS Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package main

import (
	"sync"
)

type TransactionStatus int

const (
	InProgress    TransactionStatus = 1
	PendingCancel TransactionStatus = 2
)

type TransactionMap struct {
	x sync.Mutex
	m map[string]TransactionStatus
}

// NewTransactionMap creates a transaction map.
func NewTransactionMap() *TransactionMap {
	return &TransactionMap{m: make(map[string]TransactionStatus)}
}

// StartTransaction creates a new transaction keyed on the id.
// It returns false if a transaction already exists for the id.
func (t *TransactionMap) StartTransaction(id string) bool {
	t.x.Lock()
	_, found := t.m[id]
	if !found {
		t.m[id] = InProgress
	}
	t.x.Unlock()
	return !found
}

// StatusIs checks if the stored status equals the parameterized status.
func (t *TransactionMap) StatusIs(id string, status TransactionStatus) bool {
	t.x.Lock()
	s, found := t.m[id]
	isEqual := found && s == status
	t.x.Unlock()
	return isEqual
}

// SetStatus sets the parameterized status for the given id.
// It returns false if no transaction exists for the id.
func (t *TransactionMap) SetStatus(id string, status TransactionStatus) bool {
	t.x.Lock()
	_, found := t.m[id]
	if found {
		t.m[id] = status
	}
	t.x.Unlock()
	return found
}

// Remove removes a transaction based on the given id.
// It returns false if no transaction existed for the id.
func (t *TransactionMap) Remove(id string) bool {
	t.x.Lock()
	_, found := t.m[id]
	if found {
		delete(t.m, id)
	}
	t.x.Unlock()
	return found
}
