// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

type fakeStore struct {
	objs    []*storage.ObjectAttrs
	deleted []string
}

func (f *fakeStore) List(context.Context) ([]*storage.ObjectAttrs, error) { return f.objs, nil }
func (f *fakeStore) DeleteLock(_ context.Context, obj string, _ int64) error {
	f.deleted = append(f.deleted, obj)
	return nil
}

func makeObj(name string, updated time.Time, size, gen int64) *storage.ObjectAttrs {
	return &storage.ObjectAttrs{Name: name, Updated: updated, Size: size, Generation: gen}
}

func TestParseFlags(t *testing.T) {
	bad := [][]string{
		{},
		{"--project-id=p", "--execute"},
		{"--project-id=p", "extra-arg", "--limit=1"},
		{"--project-id=p", "--min-age=-1h"},
		{"--project-id=p", "--min-age=0s"},
		{"--project-id=p", "--limit=-1"},
	}
	for _, args := range bad {
		if _, err := parseFlags(args); err == nil {
			t.Errorf("expected error for args %v", args)
		}
	}
}

func TestSelectTargets(t *testing.T) {
	cutoff := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	oldest := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	old := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	const (
		hexLocked   = "0e30aee22fe1"
		uuidOpen    = "10cbaa82-039b-4fb1-9f45-d8b5284f041b"
		hexRecent   = "abcdef012345"
		hexNewLock  = "fedcba987654"
	)

	objs := []*storage.ObjectAttrs{
		makeObj("terraform/state/default.tfstate", oldest, 126625, 1),
		makeObj("terraform/state/persistent-collector.tfstate", oldest, 50000, 10),
		makeObj("terraform/state/"+hexLocked+".tfstate", old, 17000, 3),
		makeObj("terraform/state/"+hexLocked+".tflock", old, 284, 33),
		makeObj("terraform/state/"+uuidOpen+".tfstate", oldest, 20000, 2),
		makeObj("terraform/state/"+hexRecent+".tfstate", recent, 15000, 4),
		makeObj("terraform/state/"+hexNewLock+".tfstate", old, 15000, 5),
		makeObj("terraform/state/"+hexNewLock+".tflock", recent, 284, 55),
		makeObj("terraform/state/nested/"+uuidOpen+".tfstate", oldest, 999, 6),
	}

	targets, skipped := selectTargets(objs, cutoff, 0)
	if skipped != 2 || len(targets) != 2 {
		t.Fatalf("got %d targets, %d skipped; want 2, 2", len(targets), skipped)
	}
	if targets[0].Name != uuidOpen || targets[0].LockObject != "" {
		t.Errorf("expected oldest workspace %s first, got %+v", uuidOpen, targets[0])
	}
	if targets[1].Name != hexLocked || targets[1].LockGeneration != 33 {
		t.Errorf("unexpected targets[1]: %+v", targets[1])
	}

	limited, _ := selectTargets(objs, cutoff, 1)
	if len(limited) != 1 || limited[0].Name != uuidOpen {
		t.Errorf("expected limit=1 -> [%s], got %+v", uuidOpen, limited)
	}
}

func TestDryRunDoesNotMutate(t *testing.T) {
	cfg, err := parseFlags([]string{"--project-id=test-proj"})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{objs: []*storage.ObjectAttrs{
		makeObj("terraform/state/default.tfstate", old, 1000, 1),
		makeObj("terraform/state/0e30aee22fe1.tfstate", old, 100, 2),
		makeObj("terraform/state/0e30aee22fe1.tflock", old, 50, 22),
	}}
	trigger := func(context.Context, string, string, string) error {
		t.Fatal("trigger must not be called in dry-run")
		return nil
	}
	var buf bytes.Buffer
	if err := run(context.Background(), cfg, store, trigger, &buf); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 0 {
		t.Fatalf("unexpected lock deletions in dry-run: %v", store.deleted)
	}
	if !strings.Contains(buf.String(), "DRY-RUN: 0e30aee22fe1") {
		t.Errorf("expected DRY-RUN output for 0e30aee22fe1, got %q", buf.String())
	}
}
