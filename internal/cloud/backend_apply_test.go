// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package cloud

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	tfe "github.com/hashicorp/go-tfe"
	mocks "github.com/hashicorp/go-tfe/mocks"
	version "github.com/hashicorp/go-version"
	"github.com/mitchellh/cli"
	gomock "go.uber.org/mock/gomock"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/backend"
	"github.com/opentofu/opentofu/internal/cloud/cloudplan"
	"github.com/opentofu/opentofu/internal/command/arguments"
	"github.com/opentofu/opentofu/internal/command/clistate"
	"github.com/opentofu/opentofu/internal/command/jsonformat"
	"github.com/opentofu/opentofu/internal/command/views"
	"github.com/opentofu/opentofu/internal/depsfile"
	"github.com/opentofu/opentofu/internal/initwd"
	"github.com/opentofu/opentofu/internal/plans"
	"github.com/opentofu/opentofu/internal/plans/planfile"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/terminal"
	"github.com/opentofu/opentofu/internal/tofu"
	tfversion "github.com/opentofu/opentofu/version"
)

func testOperationApply(t *testing.T, configDir string) (*backend.Operation, func(*testing.T) *terminal.TestOutput) {
	t.Helper()

	return testOperationApplyWithTimeout(t, configDir, 0)
}

func testOperationApplyWithTimeout(t *testing.T, configDir string, timeout time.Duration) (*backend.Operation, func(*testing.T) *terminal.TestOutput) {
	t.Helper()

	_, configLoader := initwd.MustLoadConfigForTests(t, configDir, "tests")

	streams, done := terminal.StreamsForTesting(t)
	view := views.NewView(streams)
	stateLockerView := views.NewStateLocker(arguments.ViewHuman, view)
	operationView := views.NewOperation(arguments.ViewHuman, false, view)

	// Many of our tests use an overridden "null" provider that's just in-memory
	// inside the test process, not a separate plugin on disk.
	depLocks := depsfile.NewLocks()
	depLocks.SetProviderOverridden(addrs.MustParseProviderSourceString("registry.opentofu.org/hashicorp/null"))

	return &backend.Operation{
		ConfigDir:       configDir,
		ConfigLoader:    configLoader,
		PlanRefresh:     true,
		StateLocker:     clistate.NewLocker(timeout, stateLockerView),
		Type:            backend.OperationTypeApply,
		View:            operationView,
		DependencyLocks: depLocks,
	}, done
}

func TestCloud_applyBasic(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after apply: %s", err.Error())
	}
}

func TestCloud_applyJSONBasic(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}

	op, done := testOperationApply(t, "./testdata/apply-json")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	outp := close(t)
	gotOut := outp.Stdout()

	if !strings.Contains(gotOut, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summary in output: %s", gotOut)
	}
	if !strings.Contains(gotOut, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summary in output: %s", gotOut)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after apply: %s", err.Error())
	}
}

func TestCloud_applyJSONWithOutputs(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}

	op, done := testOperationApply(t, "./testdata/apply-json-with-outputs")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	outp := close(t)
	gotOut := outp.Stdout()
	expectedSimpleOutput := `simple = [
        "some",
        "list",
    ]`
	expectedSensitiveOutput := `secret = (sensitive value)`
	expectedComplexOutput := `complex = {
        keyA = {
            someList = [
                1,
                2,
                3,
            ]
        }
        keyB = {
            someBool = true
            someStr  = "hello"
        }
    }`

	if !strings.Contains(gotOut, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summary in output: %s", gotOut)
	}
	if !strings.Contains(gotOut, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summary in output: %s", gotOut)
	}
	if !strings.Contains(gotOut, "Outputs:") {
		t.Fatalf("expected output header: %s", gotOut)
	}
	if !strings.Contains(gotOut, expectedSimpleOutput) {
		t.Fatalf("expected output: %s, got: %s", expectedSimpleOutput, gotOut)
	}
	if !strings.Contains(gotOut, expectedSensitiveOutput) {
		t.Fatalf("expected output: %s, got: %s", expectedSensitiveOutput, gotOut)
	}
	if !strings.Contains(gotOut, expectedComplexOutput) {
		t.Fatalf("expected output: %s, got: %s", expectedComplexOutput, gotOut)
	}
	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after apply: %s", err.Error())
	}
}

func TestCloud_applyCanceled(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	// Stop the run to simulate a Ctrl-C.
	run.Stop()

	<-run.Done()
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after cancelling apply: %s", err.Error())
	}
}

func TestCloud_applyWithoutPermissions(t *testing.T) {
	b, bCleanup := testBackendWithTags(t)
	defer bCleanup()

	// Create a named workspace without permissions.
	w, err := b.client.Workspaces.Create(
		context.Background(),
		b.organization,
		tfe.WorkspaceCreateOptions{
			Name: tfe.String("prod"),
		},
	)
	if err != nil {
		t.Fatalf("error creating named workspace: %v", err)
	}
	w.Permissions.CanQueueApply = false

	op, done := testOperationApply(t, "./testdata/apply")

	op.UIOut = b.CLI
	op.Workspace = "prod"

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "Insufficient rights to apply changes") {
		t.Fatalf("expected a permissions error, got: %v", errOutput)
	}
}

func TestCloud_applyWithVCS(t *testing.T) {
	b, bCleanup := testBackendWithTags(t)
	defer bCleanup()

	// Create a named workspace with a VCS.
	_, err := b.client.Workspaces.Create(
		context.Background(),
		b.organization,
		tfe.WorkspaceCreateOptions{
			Name:    tfe.String("prod"),
			VCSRepo: &tfe.VCSRepoOptions{},
		},
	)
	if err != nil {
		t.Fatalf("error creating named workspace: %v", err)
	}

	op, done := testOperationApply(t, "./testdata/apply")

	op.Workspace = "prod"

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "not allowed for workspaces with a VCS") {
		t.Fatalf("expected a VCS error, got: %v", errOutput)
	}
}

func TestCloud_applyWithParallelism(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")

	if b.ContextOpts == nil {
		b.ContextOpts = &tofu.ContextOpts{}
	}
	b.ContextOpts.Parallelism = 3
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "parallelism values are currently not supported") {
		t.Fatalf("expected a parallelism error, got: %v", errOutput)
	}
}

// Apply with local plan file should fail.
func TestCloud_applyWithLocalPlan(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")

	op.PlanFile = planfile.NewWrappedLocal(&planfile.Reader{})
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "saved local plan is not supported") {
		t.Fatalf("expected a saved plan error, got: %v", errOutput)
	}
}

// Apply with bookmark to an existing cloud plan that's in a confirmable state
// should work.
func TestCloud_applyWithCloudPlan(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-json")
	defer done(t)

	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	// Perform the plan before trying to apply it
	ws, err := b.client.Workspaces.Read(context.Background(), b.organization, b.WorkspaceMapping.Name)
	if err != nil {
		t.Fatalf("Couldn't read workspace: %s", err)
	}

	planRun, err := b.plan(context.Background(), context.Background(), context.Background(), op, ws)
	if err != nil {
		t.Fatalf("Couldn't perform plan: %s", err)
	}

	// Synthesize a cloud plan file with the plan's run ID
	pf := &cloudplan.SavedPlanBookmark{
		RemotePlanFormat: 1,
		RunID:            planRun.ID,
		Hostname:         b.hostname,
	}
	op.PlanFile = planfile.NewWrappedCloud(pf)

	// Start spying on the apply output (now that the plan's done)
	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}

	// Try apply
	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := close(t)
	if run.Result != backend.OperationSuccess {
		t.Fatal("expected apply operation to succeed")
	}
	if run.PlanEmpty {
		t.Fatalf("expected plan to not be empty")
	}

	gotOut := output.Stdout()
	if !strings.Contains(gotOut, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summary in output: %s", gotOut)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after apply: %s", err.Error())
	}
}

func TestCloud_applyWithoutRefresh(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	op.PlanRefresh = false
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected plan to be non-empty")
	}

	// We should find a run inside the mock client that has refresh set
	// to false.
	runsAPI := b.client.Runs.(*MockRuns)
	if got, want := len(runsAPI.Runs), 1; got != want {
		t.Fatalf("wrong number of runs in the mock client %d; want %d", got, want)
	}
	for _, run := range runsAPI.Runs {
		if diff := cmp.Diff(false, run.Refresh); diff != "" {
			t.Errorf("wrong Refresh setting in the created run\n%s", diff)
		}
	}
}

func TestCloud_applyWithRefreshOnly(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	op.PlanMode = plans.RefreshOnlyMode
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected plan to be non-empty")
	}

	// We should find a run inside the mock client that has refresh-only set
	// to true.
	runsAPI := b.client.Runs.(*MockRuns)
	if got, want := len(runsAPI.Runs), 1; got != want {
		t.Fatalf("wrong number of runs in the mock client %d; want %d", got, want)
	}
	for _, run := range runsAPI.Runs {
		if diff := cmp.Diff(true, run.RefreshOnly); diff != "" {
			t.Errorf("wrong RefreshOnly setting in the created run\n%s", diff)
		}
	}
}

func TestCloud_applyWithTarget(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	addr, _ := addrs.ParseAbsResourceStr("null_resource.foo")

	op.Targets = []addrs.Targetable{addr}
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatal("expected apply operation to succeed")
	}
	if run.PlanEmpty {
		t.Fatalf("expected plan to be non-empty")
	}

	// We should find a run inside the mock client that has the same
	// target address we requested above.
	runsAPI := b.client.Runs.(*MockRuns)
	if got, want := len(runsAPI.Runs), 1; got != want {
		t.Fatalf("wrong number of runs in the mock client %d; want %d", got, want)
	}
	for _, run := range runsAPI.Runs {
		if diff := cmp.Diff([]string{"null_resource.foo"}, run.TargetAddrs); diff != "" {
			t.Errorf("wrong TargetAddrs in the created run\n%s", diff)
		}
	}
}

// Applying with an exclude flag should error
func TestCloud_applyWithExclude(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")

	addr, _ := addrs.ParseAbsResourceStr("null_resource.foo")

	op.Workspace = testBackendSingleWorkspaceName
	op.Excludes = []addrs.Targetable{addr}

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "-exclude option is not supported") {
		t.Fatalf("expected -exclude option is not supported error, got: %v", errOutput)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after failed apply: %s", err.Error())
	}
}

func TestCloud_applyWithReplace(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	addr, _ := addrs.ParseAbsResourceInstanceStr("null_resource.foo")

	op.ForceReplace = []addrs.AbsResourceInstance{addr}
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatal("expected plan operation to succeed")
	}
	if run.PlanEmpty {
		t.Fatalf("expected plan to be non-empty")
	}

	// We should find a run inside the mock client that has the same
	// refresh address we requested above.
	runsAPI := b.client.Runs.(*MockRuns)
	if got, want := len(runsAPI.Runs), 1; got != want {
		t.Fatalf("wrong number of runs in the mock client %d; want %d", got, want)
	}
	for _, run := range runsAPI.Runs {
		if diff := cmp.Diff([]string{"null_resource.foo"}, run.ReplaceAddrs); diff != "" {
			t.Errorf("wrong ReplaceAddrs in the created run\n%s", diff)
		}
	}
}

func TestCloud_applyWithRequiredVariables(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-variables")
	defer done(t)

	op.Variables = testVariables(tofu.ValueFromNamedFile, "foo") // "bar" variable value missing
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	// The usual error of a required variable being missing is deferred and the operation
	// is successful
	if run.Result != backend.OperationSuccess {
		t.Fatal("expected plan operation to succeed")
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("unexpected TFC header in output: %s", output)
	}
}

func TestCloud_applyNoConfig(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/empty")

	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "configuration files found") {
		t.Fatalf("expected configuration files error, got: %v", errOutput)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after failed apply: %s", err.Error())
	}
}

func TestCloud_applyNoChanges(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-no-changes")
	defer done(t)

	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "No changes. Infrastructure is up-to-date.") {
		t.Fatalf("expected no changes in plan summery: %s", output)
	}
	if !strings.Contains(output, "Sentinel Result: true") {
		t.Fatalf("expected policy check result in output: %s", output)
	}
}

func TestCloud_applyNoApprove(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")

	input := testInput(t, map[string]string{
		"approve": "no",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	output := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	errOutput := output.Stderr()
	if !strings.Contains(errOutput, "Apply discarded") {
		t.Fatalf("expected an apply discarded error, got: %v", errOutput)
	}
}

func TestCloud_applyAutoApprove(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()
	ctrl := gomock.NewController(t)

	applyMock := mocks.NewMockApplies(ctrl)
	// This needs three new lines because we check for a minimum of three lines
	// in the parsing of logs in `opApply` function.
	logs := strings.NewReader(applySuccessOneResourceAdded)
	applyMock.EXPECT().Logs(gomock.Any(), gomock.Any()).Return(logs, nil)
	b.client.Applies = applyMock

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "no",
	})

	op.AutoApprove = true
	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) != 1 {
		t.Fatalf("expected an unused answer, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyApprovedExternally(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "wait-for-external-update",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	ctx := context.Background()

	run, err := b.Operation(ctx, op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	// Wait 50 milliseconds to make sure the run started.
	time.Sleep(50 * time.Millisecond)

	wl, err := b.client.Workspaces.List(
		ctx,
		b.organization,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error listing workspaces: %v", err)
	}
	if len(wl.Items) != 1 {
		t.Fatalf("expected 1 workspace, got %d workspaces", len(wl.Items))
	}

	rl, err := b.client.Runs.List(ctx, wl.Items[0].ID, nil)
	if err != nil {
		t.Fatalf("unexpected error listing runs: %v", err)
	}
	if len(rl.Items) != 1 {
		t.Fatalf("expected 1 run, got %d runs", len(rl.Items))
	}

	err = b.client.Runs.Apply(context.Background(), rl.Items[0].ID, tfe.RunApplyOptions{})
	if err != nil {
		t.Fatalf("unexpected error approving run: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "approved using the UI or API") {
		t.Fatalf("expected external approval in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyDiscardedExternally(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "wait-for-external-update",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	ctx := context.Background()

	run, err := b.Operation(ctx, op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	// Wait 50 milliseconds to make sure the run started.
	time.Sleep(50 * time.Millisecond)

	wl, err := b.client.Workspaces.List(
		ctx,
		b.organization,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error listing workspaces: %v", err)
	}
	if len(wl.Items) != 1 {
		t.Fatalf("expected 1 workspace, got %d workspaces", len(wl.Items))
	}

	rl, err := b.client.Runs.List(ctx, wl.Items[0].ID, nil)
	if err != nil {
		t.Fatalf("unexpected error listing runs: %v", err)
	}
	if len(rl.Items) != 1 {
		t.Fatalf("expected 1 run, got %d runs", len(rl.Items))
	}

	err = b.client.Runs.Discard(context.Background(), rl.Items[0].ID, tfe.RunDiscardOptions{})
	if err != nil {
		t.Fatalf("unexpected error discarding run: %v", err)
	}

	<-run.Done()
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "discarded using the UI or API") {
		t.Fatalf("expected external discard output: %s", output)
	}
	if strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("unexpected apply summery in output: %s", output)
	}
}

func TestCloud_applyWithAutoApprove(t *testing.T) {
	b, bCleanup := testBackendWithTags(t)
	defer bCleanup()
	ctrl := gomock.NewController(t)

	applyMock := mocks.NewMockApplies(ctrl)
	// This needs three new lines because we check for a minimum of three lines
	// in the parsing of logs in `opApply` function.
	logs := strings.NewReader(applySuccessOneResourceAdded)
	applyMock.EXPECT().Logs(gomock.Any(), gomock.Any()).Return(logs, nil)
	b.client.Applies = applyMock

	// Create a named workspace that auto applies.
	_, err := b.client.Workspaces.Create(
		context.Background(),
		b.organization,
		tfe.WorkspaceCreateOptions{
			Name: tfe.String("prod"),
		},
	)
	if err != nil {
		t.Fatalf("error creating named workspace: %v", err)
	}

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = "prod"
	op.AutoApprove = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) != 1 {
		t.Fatalf("expected an unused answer, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyForceLocal(t *testing.T) {
	// Set TF_FORCE_LOCAL_BACKEND so the cloud backend will use
	// the local backend with itself as embedded backend.
	t.Setenv("TF_FORCE_LOCAL_BACKEND", "1")

	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	streams, done := terminal.StreamsForTesting(t)
	view := views.NewOperation(arguments.ViewHuman, false, views.NewView(streams))
	op.View = view

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("unexpected TFC header in output: %s", output)
	}
	if output := done(t).Stdout(); !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summary in output: %s", output)
	}
	if !run.State.HasManagedResourceInstanceObjects() {
		t.Fatalf("expected resources in state")
	}
}

func TestCloud_applyWorkspaceWithoutOperations(t *testing.T) {
	b, bCleanup := testBackendWithTags(t)
	defer bCleanup()

	ctx := context.Background()

	// Create a named workspace that doesn't allow operations.
	_, err := b.client.Workspaces.Create(
		ctx,
		b.organization,
		tfe.WorkspaceCreateOptions{
			Name: tfe.String("no-operations"),
		},
	)
	if err != nil {
		t.Fatalf("error creating named workspace: %v", err)
	}

	op, done := testOperationApply(t, "./testdata/apply")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = "no-operations"

	streams, done := terminal.StreamsForTesting(t)
	view := views.NewOperation(arguments.ViewHuman, false, views.NewView(streams))
	op.View = view

	run, err := b.Operation(ctx, op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("unexpected TFC header in output: %s", output)
	}
	if output := done(t).Stdout(); !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summary in output: %s", output)
	}
	if !run.State.HasManagedResourceInstanceObjects() {
		t.Fatalf("expected resources in state")
	}
}

func TestCloud_applyLockTimeout(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	ctx := context.Background()

	// Retrieve the workspace used to run this operation in.
	w, err := b.client.Workspaces.Read(ctx, b.organization, b.WorkspaceMapping.Name)
	if err != nil {
		t.Fatalf("error retrieving workspace: %v", err)
	}

	// Create a new configuration version.
	c, err := b.client.ConfigurationVersions.Create(ctx, w.ID, tfe.ConfigurationVersionCreateOptions{})
	if err != nil {
		t.Fatalf("error creating configuration version: %v", err)
	}

	// Create a pending run to block this run.
	_, err = b.client.Runs.Create(ctx, tfe.RunCreateOptions{
		ConfigurationVersion: c,
		Workspace:            w,
	})
	if err != nil {
		t.Fatalf("error creating pending run: %v", err)
	}

	op, done := testOperationApplyWithTimeout(t, "./testdata/apply", 50*time.Millisecond)
	defer done(t)

	input := testInput(t, map[string]string{
		"cancel":  "yes",
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	_, err = b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, syscall.SIGINT)
	select {
	case <-sigint:
		// Stop redirecting SIGINT signals.
		signal.Stop(sigint)
	case <-time.After(200 * time.Millisecond):
		t.Fatalf("expected lock timeout after 50 milliseconds, waited 200 milliseconds")
	}

	if len(input.answers) != 2 {
		t.Fatalf("expected unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "Lock timeout exceeded") {
		t.Fatalf("expected lock timeout error in output: %s", output)
	}
	if strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("unexpected plan summery in output: %s", output)
	}
	if strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("unexpected apply summery in output: %s", output)
	}
}

func TestCloud_applyDestroy(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-destroy")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.PlanMode = plans.DestroyMode
	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "0 to add, 0 to change, 1 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "0 added, 0 changed, 1 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyDestroyNoConfig(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op, done := testOperationApply(t, "./testdata/empty")
	defer done(t)

	op.PlanMode = plans.DestroyMode
	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}
}

func TestCloud_applyJSONWithProvisioner(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}
	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op, done := testOperationApply(t, "./testdata/apply-json-with-provisioner")
	defer done(t)

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}

	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	outp := close(t)
	gotOut := outp.Stdout()
	if !strings.Contains(gotOut, "null_resource.foo: Provisioning with 'local-exec'") {
		t.Fatalf("expected provisioner local-exec start in logs: %s", gotOut)
	}

	if !strings.Contains(gotOut, "null_resource.foo: (local-exec):") {
		t.Fatalf("expected provisioner local-exec progress in logs: %s", gotOut)
	}

	if !strings.Contains(gotOut, "Hello World!") {
		t.Fatalf("expected provisioner local-exec output in logs: %s", gotOut)
	}

	stateMgr, _ := b.StateMgr(t.Context(), testBackendSingleWorkspaceName)
	// An error suggests that the state was not unlocked after apply
	if _, err := stateMgr.Lock(t.Context(), statemgr.NewLockInfo()); err != nil {
		t.Fatalf("unexpected error locking state after apply: %s", err.Error())
	}
}

func TestCloud_applyJSONWithProvisionerError(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}

	op, done := testOperationApply(t, "./testdata/apply-json-with-provisioner-error")
	defer done(t)

	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()

	outp := close(t)
	gotOut := outp.Stdout()

	if !strings.Contains(gotOut, "local-exec provisioner error") {
		t.Fatalf("unexpected error in apply logs: %s", gotOut)
	}
}

func TestCloud_applyPolicyPass(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-policy-passed")
	defer done(t)

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "Sentinel Result: true") {
		t.Fatalf("expected policy check result in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyPolicyHardFail(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-policy-hard-failed")

	input := testInput(t, map[string]string{
		"approve": "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	viewOutput := done(t)
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if !run.PlanEmpty {
		t.Fatalf("expected plan to be empty")
	}

	if len(input.answers) != 1 {
		t.Fatalf("expected an unused answers, got: %v", input.answers)
	}

	errOutput := viewOutput.Stderr()
	if !strings.Contains(errOutput, "hard failed") {
		t.Fatalf("expected a policy check error, got: %v", errOutput)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "Sentinel Result: false") {
		t.Fatalf("expected policy check result in output: %s", output)
	}
	if strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("unexpected apply summery in output: %s", output)
	}
}

func TestCloud_applyPolicySoftFail(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-policy-soft-failed")
	defer done(t)

	input := testInput(t, map[string]string{
		"override": "override",
		"approve":  "yes",
	})

	op.AutoApprove = false
	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) > 0 {
		t.Fatalf("expected no unused answers, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "Sentinel Result: false") {
		t.Fatalf("expected policy check result in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyPolicySoftFailAutoApproveSuccess(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()
	ctrl := gomock.NewController(t)

	policyCheckMock := mocks.NewMockPolicyChecks(ctrl)
	// This needs three new lines because we check for a minimum of three lines
	// in the parsing of logs in `opApply` function.
	logs := strings.NewReader(fmt.Sprintf("%s\n%s", sentinelSoftFail, applySuccessOneResourceAdded))

	pc := &tfe.PolicyCheck{
		ID: "pc-1",
		Actions: &tfe.PolicyActions{
			IsOverridable: true,
		},
		Permissions: &tfe.PolicyPermissions{
			CanOverride: true,
		},
		Scope:  tfe.PolicyScopeOrganization,
		Status: tfe.PolicySoftFailed,
	}
	policyCheckMock.EXPECT().Read(gomock.Any(), gomock.Any()).Return(pc, nil)
	policyCheckMock.EXPECT().Logs(gomock.Any(), gomock.Any()).Return(logs, nil)
	policyCheckMock.EXPECT().Override(gomock.Any(), gomock.Any()).Return(nil, nil)
	b.client.PolicyChecks = policyCheckMock
	applyMock := mocks.NewMockApplies(ctrl)
	// This needs three new lines because we check for a minimum of three lines
	// in the parsing of logs in `opApply` function.
	logs = strings.NewReader("\n\n\n1 added, 0 changed, 0 destroyed")
	applyMock.EXPECT().Logs(gomock.Any(), gomock.Any()).Return(logs, nil)
	b.client.Applies = applyMock

	op, done := testOperationApply(t, "./testdata/apply-policy-soft-failed")

	input := testInput(t, map[string]string{})

	op.AutoApprove = true
	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	viewOutput := done(t)
	if run.Result != backend.OperationSuccess {
		t.Fatal("expected apply operation to success due to auto-approve")
	}

	if run.PlanEmpty {
		t.Fatalf("expected plan to not be empty, plan operation completed without error")
	}

	if len(input.answers) != 0 {
		t.Fatalf("expected no answers, got: %v", input.answers)
	}

	errOutput := viewOutput.Stderr()
	if strings.Contains(errOutput, "soft failed") {
		t.Fatalf("expected no policy check errors, instead got: %v", errOutput)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Sentinel Result: false") {
		t.Fatalf("expected policy check to be false, instead got: %s", output)
	}
	if !strings.Contains(output, "Apply complete!") {
		t.Fatalf("expected apply to be complete, instead got: %s", output)
	}

	if !strings.Contains(output, "Resources: 1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected resources, instead got: %s", output)
	}
}

func TestCloud_applyPolicySoftFailAutoApprove(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()
	ctrl := gomock.NewController(t)

	applyMock := mocks.NewMockApplies(ctrl)
	// This needs three new lines because we check for a minimum of three lines
	// in the parsing of logs in `opApply` function.
	logs := strings.NewReader(applySuccessOneResourceAdded)
	applyMock.EXPECT().Logs(gomock.Any(), gomock.Any()).Return(logs, nil)
	b.client.Applies = applyMock

	// Create a named workspace that auto applies.
	_, err := b.client.Workspaces.Create(
		context.Background(),
		b.organization,
		tfe.WorkspaceCreateOptions{
			Name: tfe.String("prod"),
		},
	)
	if err != nil {
		t.Fatalf("error creating named workspace: %v", err)
	}

	op, done := testOperationApply(t, "./testdata/apply-policy-soft-failed")
	defer done(t)

	input := testInput(t, map[string]string{
		"override": "override",
		"approve":  "yes",
	})

	op.UIIn = input
	op.UIOut = b.CLI
	op.Workspace = "prod"
	op.AutoApprove = true

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result != backend.OperationSuccess {
		t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
	}
	if run.PlanEmpty {
		t.Fatalf("expected a non-empty plan")
	}

	if len(input.answers) != 2 {
		t.Fatalf("expected an unused answer, got: %v", input.answers)
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "Running apply in cloud backend") {
		t.Fatalf("expected TFC header in output: %s", output)
	}
	if !strings.Contains(output, "1 to add, 0 to change, 0 to destroy") {
		t.Fatalf("expected plan summery in output: %s", output)
	}
	if !strings.Contains(output, "Sentinel Result: false") {
		t.Fatalf("expected policy check result in output: %s", output)
	}
	if !strings.Contains(output, "1 added, 0 changed, 0 destroyed") {
		t.Fatalf("expected apply summery in output: %s", output)
	}
}

func TestCloud_applyWithRemoteError(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	op, done := testOperationApply(t, "./testdata/apply-with-error")
	defer done(t)

	op.Workspace = testBackendSingleWorkspaceName

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if run.Result.ExitStatus() != 1 {
		t.Fatalf("expected exit code 1, got %d", run.Result.ExitStatus())
	}

	output := b.CLI.(*cli.MockUi).OutputWriter.String()
	if !strings.Contains(output, "null_resource.foo: 1 error") {
		t.Fatalf("expected apply error in output: %s", output)
	}
}

func TestCloud_applyJSONWithRemoteError(t *testing.T) {
	b, bCleanup := testBackendWithName(t)
	defer bCleanup()

	stream, close := terminal.StreamsForTesting(t)

	b.renderer = &jsonformat.Renderer{
		Streams:  stream,
		Colorize: mockColorize(),
	}

	op, done := testOperationApply(t, "./testdata/apply-json-with-error")
	defer done(t)

	op.Workspace = testBackendSingleWorkspaceName

	mockSROWorkspace(t, b, op.Workspace)

	run, err := b.Operation(context.Background(), op)
	if err != nil {
		t.Fatalf("error starting operation: %v", err)
	}

	<-run.Done()
	if run.Result == backend.OperationSuccess {
		t.Fatal("expected apply operation to fail")
	}
	if run.Result.ExitStatus() != 1 {
		t.Fatalf("expected exit code 1, got %d", run.Result.ExitStatus())
	}

	outp := close(t)
	gotOut := outp.Stdout()

	if !strings.Contains(gotOut, "Unsupported block type") {
		t.Fatalf("unexpected plan error in output: %s", gotOut)
	}
}

func TestCloud_applyVersionCheck(t *testing.T) {
	testCases := map[string]struct {
		localVersion  string
		remoteVersion string
		forceLocal    bool
		executionMode string
		wantErr       string
	}{
		"versions can be different for remote apply": {
			localVersion:  "0.14.0",
			remoteVersion: "0.13.5",
			executionMode: "remote",
		},
		"versions can be different for local apply": {
			localVersion:  "0.14.0",
			remoteVersion: "0.13.5",
			executionMode: "local",
		},
		"force local with remote operations and different versions is acceptable": {
			localVersion:  "0.14.0",
			remoteVersion: "0.14.0-acme-provider-bundle",
			forceLocal:    true,
			executionMode: "remote",
		},
		"no error if versions are identical": {
			localVersion:  "0.14.0",
			remoteVersion: "0.14.0",
			forceLocal:    true,
			executionMode: "remote",
		},
		"no error if force local but workspace has remote operations disabled": {
			localVersion:  "0.14.0",
			remoteVersion: "0.13.5",
			forceLocal:    true,
			executionMode: "local",
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			b, bCleanup := testBackendWithName(t)
			defer bCleanup()

			// SETUP: Save original local version state and restore afterwards
			p := tfversion.Prerelease
			v := tfversion.Version
			s := tfversion.SemVer
			defer func() {
				tfversion.Prerelease = p
				tfversion.Version = v
				tfversion.SemVer = s
			}()

			// SETUP: Set local version for the test case
			tfversion.Prerelease = ""
			tfversion.Version = tc.localVersion
			tfversion.SemVer = version.Must(version.NewSemver(tc.localVersion))

			// SETUP: Set force local for the test case
			b.forceLocal = tc.forceLocal

			ctx := context.Background()

			// SETUP: set the operations and Terraform Version fields on the
			// remote workspace
			_, err := b.client.Workspaces.Update(
				ctx,
				b.organization,
				b.WorkspaceMapping.Name,
				tfe.WorkspaceUpdateOptions{
					ExecutionMode:    tfe.String(tc.executionMode),
					TerraformVersion: tfe.String(tc.remoteVersion),
				},
			)
			if err != nil {
				t.Fatalf("error creating named workspace: %v", err)
			}

			// RUN: prepare the apply operation and run it
			op, opDone := testOperationApply(t, "./testdata/apply")
			defer opDone(t)

			streams, done := terminal.StreamsForTesting(t)
			view := views.NewOperation(arguments.ViewHuman, false, views.NewView(streams))
			op.View = view

			input := testInput(t, map[string]string{
				"approve": "yes",
			})

			op.UIIn = input
			op.UIOut = b.CLI
			op.Workspace = testBackendSingleWorkspaceName

			run, err := b.Operation(ctx, op)
			if err != nil {
				t.Fatalf("error starting operation: %v", err)
			}

			// RUN: wait for completion
			<-run.Done()
			output := done(t)

			if tc.wantErr != "" {
				// ASSERT: if the test case wants an error, check for failure
				// and the error message
				if run.Result != backend.OperationFailure {
					t.Fatalf("expected run to fail, but result was %#v", run.Result)
				}
				errOutput := output.Stderr()
				if !strings.Contains(errOutput, tc.wantErr) {
					t.Fatalf("missing error %q\noutput: %s", tc.wantErr, errOutput)
				}
			} else {
				// ASSERT: otherwise, check for success and appropriate output
				// based on whether the run should be local or remote
				if run.Result != backend.OperationSuccess {
					t.Fatalf("operation failed: %s", b.CLI.(*cli.MockUi).ErrorWriter.String())
				}
				output := b.CLI.(*cli.MockUi).OutputWriter.String()
				hasRemote := strings.Contains(output, "Running apply in cloud backend")
				hasSummary := strings.Contains(output, "1 added, 0 changed, 0 destroyed")
				hasResources := run.State.HasManagedResourceInstanceObjects()
				if !tc.forceLocal && !isLocalExecutionMode(tc.executionMode) {
					if !hasRemote {
						t.Errorf("missing TFC header in output: %s", output)
					}
					if !hasSummary {
						t.Errorf("expected apply summary in output: %s", output)
					}
				} else {
					if hasRemote {
						t.Errorf("unexpected TFC header in output: %s", output)
					}
					if !hasResources {
						t.Errorf("expected resources in state")
					}
				}
			}
		})
	}
}

const applySuccessOneResourceAdded = `
OpenTofu v0.11.10

Initializing plugins and modules...
null_resource.hello: Creating...
null_resource.hello: Creation complete after 0s (ID: 8657651096157629581)

Apply complete! Resources: 1 added, 0 changed, 0 destroyed.
`

const sentinelSoftFail = `
Sentinel Result: false

Sentinel evaluated to false because one or more Sentinel policies evaluated
to false. This false was not due to an undefined value or runtime error.

1 policies evaluated.

## Policy 1: Passthrough.sentinel (soft-mandatory)

Result: false

FALSE - Passthrough.sentinel:1:1 - Rule "main"
`
