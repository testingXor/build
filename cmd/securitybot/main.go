// Copyright 2022 Go Authors All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/build/buildenv"
	"golang.org/x/build/buildlet"
	"golang.org/x/build/dashboard"
	"golang.org/x/build/gerrit"
	"golang.org/x/build/internal/gomote/protos"
	"golang.org/x/build/internal/iapclient"
	"golang.org/x/build/types"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"cloud.google.com/go/storage"
)

type tester struct {
	source string
	repo   string

	coordinator *buildlet.GRPCCoordinatorClient
	gcs         *storage.Client
	http        *http.Client
	gerrit      *gerrit.Client
}

// runTests creates a buildlet for the specified builderType, sends a copy of go1.4 and the change tarball to
// the buildlet, and then executes the platform specific 'all' script, streaming the output to a GCS bucket.
// The buildlet is destroyed on return. The bool returned indicates whether the tests passed or failed.
func (t *tester) runTests(ctx context.Context, builderType string, rev string, archive []byte) (string, bool) {
	log.Printf("%s: creating buildlet", builderType)
	c, err := t.coordinator.CreateBuildletWithStatus(ctx, builderType, func(status types.BuildletWaitStatus) {})
	if err != nil {
		log.Printf("%s: failed to create buildlet: %s", builderType, err)
		return "", false
	}
	buildletName := c.RemoteName()
	log.Printf("%s: created buildlet (%s)", builderType, buildletName)
	defer func() {
		if err := c.Close(); err != nil {
			log.Printf("%s: unable to close buildlet %q: %s", builderType, buildletName, err)
		} else {
			log.Printf("%s: destroyed buildlet", builderType)
		}
	}()

	buildConfig, ok := dashboard.Builders[builderType]
	if !ok {
		log.Printf("%s: unknown builder type", builderType)
		return "", false
	}
	bootstrapURL := buildConfig.GoBootstrapURL(buildenv.Production)
	// Assume if bootstrapURL == "" the buildlet is already bootstrapped
	if bootstrapURL != "" {
		if err := c.PutTarFromURL(ctx, bootstrapURL, "go1.4"); err != nil {
			log.Printf("%s: failed to bootstrap buildlet: %s", builderType, err)
			return "", false
		}
	}

	if err := c.PutTar(ctx, bytes.NewReader(archive), "go"); err != nil {
		log.Printf("%s: failed to upload change archive: %s", builderType, err)
		return "", false
	}
	if err := c.Put(ctx, strings.NewReader("devel "+rev), "go/VERSION", 0644); err != nil {
		log.Printf("%s: failed to upload VERSION file: %s", builderType, err)
		return "", false
	}

	suffix := make([]byte, 4)
	rand.Read(suffix)

	var output io.Writer
	var logURL string

	if t.gcs != nil {
		gcsBucket, gcsObject := *gcsBucket, fmt.Sprintf("%s-%x/%s", rev, suffix, builderType)
		gcsWriter, err := newLiveWriter(ctx, t.gcs.Bucket(gcsBucket).Object(gcsObject))
		if err != nil {
			log.Printf("%s: failed to create live writer: %s", builderType, err)
			return "", false
		}
		defer func() {
			if err := gcsWriter.Close(); err != nil {
				log.Printf("%s: failed to flush GCS writer: %s", builderType, err)
			}
		}()
		logURL = "https://storage.cloud.google.com/" + path.Join(gcsBucket, gcsObject)
		output = gcsWriter
	} else {
		output = &localWriter{buildletName}
	}

	work, err := c.WorkDir(ctx)
	if err != nil {
		log.Printf("%s: failed to retrieve work dir: %s", builderType, err)
		return "", false
	}

	env := append(buildConfig.Env(), "GOPATH="+work+"/gopath", "GOROOT_FINAL="+buildConfig.GorootFinal())
	cmd, args := "go/"+buildConfig.AllScript(), buildConfig.AllScriptArgs()
	opts := buildlet.ExecOpts{
		Output:   output,
		ExtraEnv: env,
		Args:     args,
		OnStartExec: func() {
			log.Printf("%s: starting all.bash %s", builderType, logURL)
		},
	}
	remoteErr, execErr := c.Exec(ctx, cmd, opts)
	if execErr != nil {
		log.Printf("%s: failed to execute all.bash: %s", builderType, execErr)
		return logURL, false
	}
	if remoteErr != nil {
		log.Printf("%s: tests failed: %s", builderType, remoteErr)
		return logURL, false
	}
	log.Printf("%s: tests succeeded", builderType)
	return logURL, true
}

// gcsLiveWriter is an extremely hacky way of getting live(ish) updating logs while
// using GCS. The buffer is written out to an object every 5 seconds.
type gcsLiveWriter struct {
	obj  *storage.ObjectHandle
	buf  *bytes.Buffer
	mu   *sync.Mutex
	stop chan bool
	err  chan error
}

func newLiveWriter(ctx context.Context, obj *storage.ObjectHandle) (*gcsLiveWriter, error) {
	stopCh, errCh := make(chan bool, 1), make(chan error, 1)
	mu := new(sync.Mutex)
	buf := new(bytes.Buffer)
	write := func(b []byte) error {
		w := obj.NewWriter(ctx)
		w.Write(b)
		if err := w.Close(); err != nil {
			return err
		}
		return nil
	}
	if err := write([]byte{}); err != nil {
		return nil, err
	}
	go func() {
		t := time.NewTicker(time.Second * 5)
		for {
			select {
			case <-stopCh:
				mu.Lock()
				errCh <- write(buf.Bytes())
				mu.Unlock()
			case <-t.C:
				mu.Lock()
				if err := write(buf.Bytes()); err != nil {
					log.Printf("GCS write to %q failed! %s", path.Join(obj.BucketName(), obj.ObjectName()), err)
					errCh <- err
				}
				mu.Unlock()
			}
		}
	}()
	return &gcsLiveWriter{obj: obj, buf: buf, mu: mu, stop: stopCh, err: errCh}, nil
}

func (g *gcsLiveWriter) Write(b []byte) (int, error) {
	g.mu.Lock()
	g.buf.Write(b)
	g.mu.Unlock()
	return len(b), nil
}

func (g *gcsLiveWriter) Close() error {
	g.stop <- true
	return <-g.err
}

type localWriter struct {
	buildlet string
}

func (lw *localWriter) Write(b []byte) (int, error) {
	prefix := []byte(lw.buildlet + ": ")
	var prefixed []byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		prefixed = append(prefixed, append(prefix, append(l, byte('\n'))...)...)
	}

	return os.Stdout.Write(prefixed)
}

// getTar retrieves the tarball for a specific git revision from t.source and returns
// the bytes.
func (t *tester) getTar(revision string) ([]byte, error) {
	tarURL := t.source + "/" + t.repo + "/+archive/" + revision + ".tar.gz"
	req, err := http.NewRequest("GET", tarURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch %q: %v", tarURL, resp.Status)
	}
	defer resp.Body.Close()
	archive, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Check what we got back was actually the archive, since Google's SSO page will
	// return 200.
	_, err = gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}

	return archive, nil
}

type result struct {
	builderType string
	logURL      string
	succeeded   bool
}

// run tests the specific revision on the builders specified.
func (t *tester) run(ctx context.Context, revision string, builders []string) ([]result, error) {
	archive, err := t.getTar(revision)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve change archive: %s", err)
	}

	wg := new(sync.WaitGroup)
	resultsCh := make(chan result, len(builders))
	for _, bt := range builders {
		wg.Add(1)
		go func(bt string) {
			defer wg.Done()
			log, succeeded := t.runTests(ctx, bt, revision, archive) // have a proper timeout
			resultsCh <- result{bt, log, succeeded}
		}(bt)
	}
	wg.Wait()
	close(resultsCh)
	results := make([]result, 0, len(builders))
	for result := range resultsCh {
		results = append(results, result)
	}

	return results, nil
}

// commentBeginning send the review message indicating the trybots are beginning.
func (t *tester) commentBeginning(ctx context.Context, change *gerrit.ChangeInfo) error {
	// It would be nice to do a similar thing to the coordinator, using comment
	// threads that can be resolved, but that is slightly more complex than what
	// we really need to start with.
	//
	// Similarly it would be nice to comment links to logs earlier.
	return t.gerrit.SetReview(ctx, change.ID, change.CurrentRevision, gerrit.ReviewInput{
		Message: "TryBots beginning",
	})
}

// commentResults sends the review message containing the results for the change
// and applies the TryBot-Result label.
func (t *tester) commentResults(ctx context.Context, change *gerrit.ChangeInfo, results []result) error {
	state := "succeeded"
	label := 1
	buf := new(bytes.Buffer)
	w := tabwriter.NewWriter(buf, 0, 0, 1, ' ', 0)
	for _, res := range results {
		s := "pass"
		if !res.succeeded {
			s = "failed"
			state = "failed"
			label = -1
		}
		fmt.Fprintf(w, "    %s\t[%s]\t%s\n", res.builderType, s, res.logURL)
	}
	w.Flush()

	comment := fmt.Sprintf("Tests %s\n%s", state, buf.String())
	if err := t.gerrit.SetReview(ctx, change.ID, change.CurrentRevision, gerrit.ReviewInput{
		Message: comment,
		Labels:  map[string]int{"TryBot-Result": label},
	}); err != nil {
		return err
	}

	return nil
}

// findCharges queries a gerrit instance for changes which should be tested, returning a
// slice of revisions for each change.
func (t *tester) findChanges(ctx context.Context) ([]*gerrit.ChangeInfo, error) {
	return t.gerrit.QueryChanges(
		ctx,
		fmt.Sprintf("project:%s status:open label:Run-TryBot+1 -label:TryBot-Result-1 -label:TryBot-Result+1", t.repo),
		gerrit.QueryChangesOpt{Fields: []string{"CURRENT_REVISION"}},
	)
}

var (
	username = flag.String("user", "user-security", "Coordinator username")

	gerritURL = flag.String("gerrit", "https://team-review.googlesource.com", "URL for the gerrit instance")
	sourceURL = flag.String("source", "https://team.googlesource.com", "URL for the source instance")
	repoName  = flag.String("repo", "golang/go-private", "Gerrit repository name")

	gcsBucket = flag.String("gcs", "", "GCS bucket path for logs")

	revision    = flag.String("revision", "", "Revision to test, when running in one-shot mode")
	buildersStr = flag.String("builders", "", "Comma separated list of builder types to test against by default")
)

// allowedBuilders contains the set of builders which are acceptable to use for testing
// PRIVATE track security changes. These builders should, generally, be controlled by
// Google.
var allowedBuilders = map[string]bool{
	"js-wasm": true,

	"linux-386":            true,
	"linux-386-longtest":   true,
	"linux-amd64":          true,
	"linux-amd64-longtest": true,

	"linux-amd64-bullseye": true,

	"darwin-amd64-12_0": true,
	"darwin-arm64-12":   true,

	"windows-386-2012":   true,
	"windows-amd64-2016": true,
	"windows-arm64-11":   true,
}

// firstClassBuilders is the default set of builders to test against,
// representing the first class ports as defined by the port policy.
var firstClassBuilders = []string{
	"linux-386-longtest",
	"linux-amd64-longtest",
	"linux-arm-aws",
	"linux-arm64-aws",

	"darwin-amd64-12_0",
	"darwin-arm64-12",

	"windows-386-2012",
	"windows-amd64-longtest",
}

func main() {
	flag.Parse()
	ctx := context.Background()

	creds, err := google.FindDefaultCredentials(ctx, gerrit.OAuth2Scopes...)
	if err != nil {
		log.Fatalf("reading GCP credentials: %v", err)
	}
	gerritClient := gerrit.NewClient(*gerritURL, gerrit.OAuth2Auth(creds.TokenSource))
	httpClient := oauth2.NewClient(ctx, creds.TokenSource)

	var builders []string
	if *buildersStr != "" {
		for _, b := range strings.Split(*buildersStr, ",") {
			if !allowedBuilders[b] {
				log.Fatalf("builder type %q not allowed", b)
			}
			builders = append(builders, b)
		}

	} else {
		builders = firstClassBuilders
	}

	var gcsClient *storage.Client
	if *gcsBucket != "" {
		gcsClient, err = storage.NewClient(ctx)
		if err != nil {
			log.Fatalf("Could not connect to GCS: %v", err)
		}
	}

	cc, err := iapclient.GRPCClient(ctx, "build.golang.org:443")
	if err != nil {
		log.Fatalf("Could not connect to coordinator: %v", err)
	}
	b := buildlet.GRPCCoordinatorClient{
		Client: protos.NewGomoteServiceClient(cc),
	}

	t := &tester{
		source:      strings.TrimSuffix(*sourceURL, "/"),
		repo:        *repoName,
		coordinator: &b,
		http:        httpClient,
		gcs:         gcsClient,
		gerrit:      gerritClient,
	}

	if *revision != "" {
		if _, err := t.run(ctx, *revision, builders); err != nil {
			log.Fatal(err)
		}
	} else {
		ticker := time.NewTicker(time.Minute)
		for range ticker.C {
			changes, err := t.findChanges(context.Background())
			if err != nil {
				log.Fatalf("findChanges failed: %v", err)
			}
			log.Printf("found %d changes", len(changes))

			for _, change := range changes {
				log.Printf("testing CL %d patchset %d (%s)", change.ChangeNumber, change.Revisions[change.CurrentRevision].PatchSetNumber, change.CurrentRevision)
				if err := t.commentBeginning(context.Background(), change); err != nil {
					log.Fatalf("commentBeginning failed: %v", err)
				}
				results, err := t.run(ctx, change.CurrentRevision, builders)
				if err != nil {
					log.Fatalf("run failed: %v", err)
				}
				if err := t.commentResults(context.Background(), change, results); err != nil {
					log.Fatalf("commentResults failed: %v", err)
				}
			}
		}
	}
}