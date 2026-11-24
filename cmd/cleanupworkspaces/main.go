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

// Command cleanupworkspaces discovers stale Terraform workspaces in the e2e
// GCS state bucket and cleans them up by invoking the global-e2e-cleanup
// Cloud Build trigger. Dry-run by default; pass --execute to run.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/cloudbuild/v1"
	"google.golang.org/api/iterator"
)

const (
	defaultWorkspace = "default"
	statePrefix      = "terraform/state/"
	cleanupTrigger   = "global-e2e-cleanup"
	maxConcurrency   = 10
	pollInterval     = 15 * time.Second
)

// ephemeralWorkspaceRe matches only ephemeral e2e test run IDs: legacy 12-char
// hex IDs or Cloud Build UUIDs. Named workspaces (e.g. "default") never match.
var ephemeralWorkspaceRe = regexp.MustCompile(`^([0-9a-f]{12}|[0-9a-f-]{36})$`)

type config struct {
	projectID string
	image     string
	minAge    time.Duration
	limit     int
	execute   bool
}

type workspaceTarget struct {
	Name           string
	StateUpdated   time.Time
	StateSize      int64
	LockObject     string
	LockGeneration int64
	LockUpdated    time.Time
}

type stateStore interface {
	List(ctx context.Context) ([]*storage.ObjectAttrs, error)
	DeleteLock(ctx context.Context, object string, generation int64) error
}

type gcsStore struct {
	bucket *storage.BucketHandle
}

func (s *gcsStore) List(ctx context.Context) ([]*storage.ObjectAttrs, error) {
	var out []*storage.ObjectAttrs
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: statePrefix, Delimiter: "/"})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if attrs.Name != "" {
			out = append(out, attrs)
		}
	}
}

func (s *gcsStore) DeleteLock(ctx context.Context, object string, generation int64) error {
	if generation <= 0 {
		return fmt.Errorf("refusing unconditional lock delete for %s: invalid generation %d", object, generation)
	}
	return s.bucket.Object(object).If(storage.Conditions{GenerationMatch: generation}).Delete(ctx)
}

type triggerRunner func(ctx context.Context, projectID, image, workspace string) error

func newCloudBuildTriggerRunner(cb *cloudbuild.Service) triggerRunner {
	return func(ctx context.Context, projectID, image, workspace string) error {
		op, err := cb.Projects.Triggers.Run(projectID, cleanupTrigger, &cloudbuild.RepoSource{
			Substitutions: map[string]string{
				"_TEST_RUN_ID":       workspace,
				"_TEST_RUNNER_IMAGE": image,
				"_BUILD_STATUS":      "FAILURE",
			},
		}).Context(ctx).Do()
		if err != nil {
			return err
		}
		opName := op.Name
		var meta struct {
			Build struct {
				ID     string `json:"id"`
				LogURL string `json:"logUrl"`
			} `json:"build"`
		}
		_ = json.Unmarshal(op.Metadata, &meta)
		log.Printf("STARTED workspace=%s build=%s url=%s", workspace, meta.Build.ID, meta.Build.LogURL)
		for !op.Done {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
			op, err = cb.Operations.Get(opName).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("polling %s (url=%s): %w", opName, meta.Build.LogURL, err)
			}
		}
		if op.Error != nil {
			return fmt.Errorf("build failed (build=%s url=%s): %s", meta.Build.ID, meta.Build.LogURL, op.Error.Message)
		}
		return nil
	}
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		log.Fatalf("invalid flags: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	storageClient, err := storage.NewClient(ctx)
	if err != nil {
		log.Fatalf("creating storage client: %v", err)
	}
	defer storageClient.Close()

	var trigger triggerRunner
	if cfg.execute {
		cb, err := cloudbuild.NewService(ctx)
		if err != nil {
			log.Fatalf("creating cloudbuild service: %v", err)
		}
		trigger = newCloudBuildTriggerRunner(cb)
	}

	store := &gcsStore{bucket: storageClient.Bucket(cfg.projectID + "-e2e-tfstate")}
	if err := run(ctx, cfg, store, trigger, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func parseFlags(args []string) (config, error) {
	fs := flag.NewFlagSet("cleanupworkspaces", flag.ContinueOnError)
	var cfg config
	fs.StringVar(&cfg.projectID, "project-id", "", "GCP project ID (required)")
	fs.StringVar(&cfg.image, "image", "", "Test runner container image (required with --execute)")
	fs.DurationVar(&cfg.minAge, "min-age", 7*24*time.Hour, "Minimum age of state/lock objects before cleanup")
	fs.IntVar(&cfg.limit, "limit", 0, "Maximum number of workspaces to process (0 for no limit)")
	fs.BoolVar(&cfg.execute, "execute", false, "Clear stale locks and invoke cleanup builds (default is dry-run)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if fs.NArg() > 0 {
		return cfg, fmt.Errorf("unexpected positional arguments: %v", fs.Args())
	}
	if cfg.projectID = strings.TrimSpace(cfg.projectID); cfg.projectID == "" || strings.ContainsAny(cfg.projectID, " \t\r\n") {
		return cfg, errors.New("--project-id is required")
	}
	if cfg.image = strings.TrimSpace(cfg.image); cfg.execute && (cfg.image == "" || strings.ContainsAny(cfg.image, " \t\r\n")) {
		return cfg, errors.New("--image is required when --execute is set")
	}
	if cfg.minAge <= 0 {
		return cfg, fmt.Errorf("--min-age must be > 0, got %s", cfg.minAge)
	}
	if cfg.limit < 0 {
		return cfg, fmt.Errorf("--limit must be >= 0, got %d", cfg.limit)
	}
	return cfg, nil
}

func run(ctx context.Context, cfg config, store stateStore, trigger triggerRunner, out io.Writer) error {
	objects, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("listing state objects: %w", err)
	}

	targets, skipped := selectTargets(objects, time.Now().UTC().Add(-cfg.minAge), cfg.limit)
	log.Printf("Selected %d stale workspace(s) older than %s (skipped %d recent; %q excluded)", len(targets), cfg.minAge, skipped, defaultWorkspace)

	if cfg.execute {
		return executeCleanup(ctx, cfg, store, trigger, targets)
	}
	printDryRun(out, cfg.projectID, targets)
	return nil
}

func printDryRun(out io.Writer, projectID string, targets []workspaceTarget) {
	bucket := projectID + "-e2e-tfstate"
	fmt.Fprintf(out, "DRY-RUN: project=%s trigger=%s\n", projectID, cleanupTrigger)
	for _, t := range targets {
		lock := ""
		if t.LockObject != "" {
			lock = fmt.Sprintf(" [DELETE LOCK gs://%s/%s#%d (since %s)]", bucket, t.LockObject, t.LockGeneration, t.LockUpdated.Format(time.RFC3339))
		}
		fmt.Fprintf(out, "DRY-RUN: %s (updated=%s size=%d)%s\n", t.Name, t.StateUpdated.Format(time.RFC3339), t.StateSize, lock)
	}
}

func executeCleanup(ctx context.Context, cfg config, store stateStore, trigger triggerRunner, targets []workspaceTarget) error {
	if len(targets) == 0 {
		return nil
	}

	var (
		g      errgroup.Group
		done   atomic.Int64
		failed atomic.Int64
	)
	g.SetLimit(maxConcurrency)
	total := int64(len(targets))

	for _, t := range targets {
		t := t
		g.Go(func() error {
			start := time.Now()
			err := cleanupOne(ctx, store, trigger, cfg.projectID, cfg.image, t)
			n := done.Add(1)
			if err != nil {
				failed.Add(1)
				log.Printf("[%d/%d] FAILED workspace=%s (%s): %v", n, total, t.Name, time.Since(start).Round(time.Second), err)
				return nil
			}
			log.Printf("[%d/%d] OK workspace=%s (%s)", n, total, t.Name, time.Since(start).Round(time.Second))
			return nil
		})
	}
	_ = g.Wait()

	failedCount := failed.Load()
	log.Printf("Done: %d succeeded, %d failed (total %d)", total-failedCount, failedCount, total)
	if failedCount > 0 {
		return fmt.Errorf("%d of %d workspace cleanup(s) failed", failedCount, total)
	}
	return nil
}

func selectTargets(objects []*storage.ObjectAttrs, cutoff time.Time, limit int) ([]workspaceTarget, int) {
	states := map[string]*storage.ObjectAttrs{}
	locks := map[string]*storage.ObjectAttrs{}

	for _, obj := range objects {
		rel, ok := strings.CutPrefix(obj.Name, statePrefix)
		if !ok {
			continue
		}
		if ws, ok := strings.CutSuffix(rel, ".tfstate"); ok && ws != "" {
			states[ws] = obj
		} else if ws, ok := strings.CutSuffix(rel, ".tflock"); ok && ws != "" {
			locks[ws] = obj
		}
	}

	var targets []workspaceTarget
	skipped := 0
	for ws, st := range states {
		if ws == defaultWorkspace || !ephemeralWorkspaceRe.MatchString(ws) {
			continue
		}
		if st.Updated.IsZero() || !st.Updated.Before(cutoff) {
			skipped++
			continue
		}
		t := workspaceTarget{Name: ws, StateUpdated: st.Updated, StateSize: st.Size}
		if lk, ok := locks[ws]; ok {
			if lk.Updated.IsZero() || !lk.Updated.Before(cutoff) {
				skipped++
				continue
			}
			t.LockObject = lk.Name
			t.LockGeneration = lk.Generation
			t.LockUpdated = lk.Updated
		}
		targets = append(targets, t)
	}

	sort.Slice(targets, func(i, j int) bool {
		if !targets[i].StateUpdated.Equal(targets[j].StateUpdated) {
			return targets[i].StateUpdated.Before(targets[j].StateUpdated)
		}
		return targets[i].Name < targets[j].Name
	})
	if limit > 0 && len(targets) > limit {
		targets = targets[:limit]
	}
	return targets, skipped
}

func cleanupOne(ctx context.Context, store stateStore, trigger triggerRunner, projectID, image string, t workspaceTarget) error {
	if t.LockObject != "" {
		if err := store.DeleteLock(ctx, t.LockObject, t.LockGeneration); err != nil {
			return fmt.Errorf("unlocking %s#%d: %w", t.LockObject, t.LockGeneration, err)
		}
	}
	return trigger(ctx, projectID, image, t.Name)
}
