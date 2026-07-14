package deployer

import (
	"context"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/hashicorp/go-tfe"
	"github.com/hashicorp/go-tfe/mocks"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func listOpts(page int) *tfe.VariableListOptions {
	return &tfe.VariableListOptions{
		ListOptions: tfe.ListOptions{PageNumber: page, PageSize: variablePageSize},
	}
}

// expectRun stubs a run that reaches "applied".
func expectRun(mr *mocks.MockRuns, wsp *tfe.Workspace) {
	mr.EXPECT().Create(gomock.Any(), tfe.RunCreateOptions{
		Message:   stringPtr("commit"),
		Workspace: wsp,
		AutoApply: boolPtr(true),
	}).Return(&tfe.Run{ID: "run-id", Status: tfe.RunPending}, nil)
	mr.EXPECT().
		Read(gomock.Any(), "run-id").
		Return(&tfe.Run{ID: "run-id", Status: tfe.RunApplied}, nil)
}

func TestDeployer_New(t *testing.T) {
	log, err := zap.NewDevelopment()
	require.NoError(t, err)
	ctx := context.Background()
	tfc, err := tfe.NewClient(&tfe.Config{
		Token: "token",
	})
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	m := mocks.NewMockWorkspaces(ctrl)
	tfc.Workspaces = m
	require.NoError(t, err)
	m.EXPECT().Read(ctx, "organization", "workspace").Return(&tfe.Workspace{}, nil)
	deployer, err := NewDeployer(ctx, log, tfc, &Config{
		Organization: "organization",
		Workspace:    "workspace",
	})
	require.NoError(t, err)
	require.NotNil(t, deployer)
}

func TestDeployer_Deploy(t *testing.T) {
	tcs := []struct {
		name    string
		config  *Config
		updates []Update
		mock    func(*mocks.MockWorkspaces, *mocks.MockVariables, *mocks.MockRuns)
		expect  func(*testing.T, error)
	}{
		{
			name: "run applied",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Value: "new-var-value"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "var-value"},
						},
					}, nil)
				mv.EXPECT().
					Update(gomock.Any(), "workspace-id", "var-id", tfe.VariableUpdateOptions{
						Value: stringPtr("new-var-value"),
					}).
					Return(nil, nil)

				expectRun(mr, wsp)
			},
			expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			// Terraform Cloud pages variables 20 at a time. A workspace big
			// enough to push the target onto page 2 used to fail with
			// "variable not found".
			name: "finds a variable on the second page",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Value: "new-var-value"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Pagination: &tfe.Pagination{CurrentPage: 1, NextPage: 2, TotalPages: 2},
						Items: []*tfe.Variable{
							{ID: "other-id", Key: "other-key", Value: "other-value"},
						},
					}, nil)
				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(2)).
					Return(&tfe.VariableList{
						Pagination: &tfe.Pagination{CurrentPage: 2, NextPage: 0, TotalPages: 2},
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "var-value"},
						},
					}, nil)
				mv.EXPECT().
					Update(gomock.Any(), "workspace-id", "var-id", tfe.VariableUpdateOptions{
						Value: stringPtr("new-var-value"),
					}).
					Return(nil, nil)

				expectRun(mr, wsp)
			},
			expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			// Every path lands in one read-modify-write and one run: a wave of
			// fifteen heralds must not mean fifteen applies of the workspace.
			name: "many paths, one variable update, one run",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{
				{Name: "herald_roster", Path: "a.image", Value: "img"},
				{Name: "herald_roster", Path: "b.image", Value: "img"},
			},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "herald_roster", HCL: true, Value: roster},
						},
					}, nil)

				mv.EXPECT().
					Update(gomock.Any(), "workspace-id", "var-id", gomock.Any()).
					DoAndReturn(func(
						_ context.Context,
						_ string,
						_ string,
						opts tfe.VariableUpdateOptions,
					) (*tfe.Variable, error) {
						require.Contains(t, *opts.Value, `image       = "img"`)
						require.NotContains(t, *opts.Value, "sha-7036d9b")
						require.Contains(t, *opts.Value, `archil_disk = "herald-dev/herald-a"`)
						require.Contains(t, *opts.Value, `archil_disk = "herald-dev/herald-b"`)
						return nil, nil
					})

				expectRun(mr, wsp)
			},
			expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			// An auto-applied run applies the whole workspace, so a redeploy of
			// the image that is already live must not drag unrelated pending
			// infrastructure changes out with it.
			name: "no run when the value is unchanged",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Value: "same-value"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "same-value"},
						},
					}, nil)

				// No Update, no Create: gomock fails the test if either fires.
			},
			expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			// Writing the variable canonicalizes its formatting, so a roster a
			// human wrote in the JSON dialect re-renders differently even when
			// no image moved. Comparing rendered text would call that a change
			// and auto-apply the whole workspace for nothing.
			name: "no run when a path update is semantically a no-op",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "herald_roster", Path: "*.image", Value: "img"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{
								ID:  "var-id",
								Key: "herald_roster",
								HCL: true,
								// Same images the update sets, different dialect
								// and whitespace to what we would render.
								Value: `{"a": {"image": "img", "archil_disk": "d-a"},
								         "b": {"image": "img", "archil_disk": "d-b"}}`,
							},
						},
					}, nil)

				// No Update, no Create: gomock fails the test if either fires.
			},
			expect: func(t *testing.T, err error) {
				require.NoError(t, err)
			},
		},
		{
			name: "variable not found is never an upsert",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "missing", Value: "v"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "var-value"},
						},
					}, nil)
			},
			expect: func(t *testing.T, err error) {
				require.ErrorContains(t, err, "variable missing not found")
			},
		},
		{
			name: "path update against a plain string variable",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Path: "a.image", Value: "img"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", HCL: false, Value: "plain"},
						},
					}, nil)
			},
			expect: func(t *testing.T, err error) {
				require.ErrorContains(t, err, "not HCL-typed")
			},
		},
		{
			name: "run canceled",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Value: "new-var-value"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "var-value"},
						},
					}, nil)
				mv.EXPECT().
					Update(gomock.Any(), "workspace-id", "var-id", tfe.VariableUpdateOptions{
						Value: stringPtr("new-var-value"),
					}).
					Return(nil, nil)

				mr.EXPECT().Create(gomock.Any(), tfe.RunCreateOptions{
					Message:   stringPtr("commit"),
					Workspace: wsp,
					AutoApply: boolPtr(true),
				}).Return(&tfe.Run{ID: "run-id", Status: tfe.RunPending}, nil)
				mr.EXPECT().Read(gomock.Any(), "run-id").Return(&tfe.Run{
					ID:     "run-id",
					Status: tfe.RunApplying,
				}, nil)
				mr.EXPECT().Read(gomock.Any(), "run-id").Return(&tfe.Run{
					ID:     "run-id",
					Status: tfe.RunCanceled,
				}, nil)
			},
			expect: func(t *testing.T, err error) {
				require.EqualError(t, err, "waiting on run: run canceled with status \"canceled\"")
			},
		},
		{
			name: "wait timeout",
			config: &Config{
				Organization: "organization",
				Workspace:    "workspace",
				WaitDelay:    1 * time.Millisecond,
				WaitTimeout:  10 * time.Millisecond,
			},
			updates: []Update{{Name: "var-key", Value: "new-var-value"}},
			mock: func(mw *mocks.MockWorkspaces, mv *mocks.MockVariables, mr *mocks.MockRuns) {
				wsp := &tfe.Workspace{ID: "workspace-id"}
				mw.EXPECT().Read(gomock.Any(), "organization", "workspace").Return(wsp, nil)

				mv.EXPECT().
					List(gomock.Any(), "workspace-id", listOpts(1)).
					Return(&tfe.VariableList{
						Items: []*tfe.Variable{
							{ID: "var-id", Key: "var-key", Value: "var-value"},
						},
					}, nil)
				mv.EXPECT().
					Update(gomock.Any(), "workspace-id", "var-id", tfe.VariableUpdateOptions{
						Value: stringPtr("new-var-value"),
					}).
					Return(nil, nil)

				mr.EXPECT().Create(gomock.Any(), tfe.RunCreateOptions{
					Message:   stringPtr("commit"),
					Workspace: wsp,
					AutoApply: boolPtr(true),
				}).Return(&tfe.Run{ID: "run-id", Status: tfe.RunPending}, nil)
				mr.EXPECT().Read(gomock.Any(), "run-id").Return(&tfe.Run{
					ID:     "run-id",
					Status: tfe.RunApplying,
				}, nil).AnyTimes()
			},
			expect: func(t *testing.T, err error) {
				require.EqualError(t, err, "waiting on run: timeout waiting for run")
			},
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			log, err := zap.NewDevelopment()
			require.NoError(t, err)
			ctx := context.Background()
			tfc, err := tfe.NewClient(&tfe.Config{
				Token: "token",
			})
			require.NoError(t, err)

			ctrl := gomock.NewController(t)

			workspaces := mocks.NewMockWorkspaces(ctrl)
			tfc.Workspaces = workspaces

			variables := mocks.NewMockVariables(ctrl)
			tfc.Variables = variables

			runs := mocks.NewMockRuns(ctrl)
			tfc.Runs = runs

			tc.mock(workspaces, variables, runs)

			deployer, err := NewDeployer(ctx, log, tfc, tc.config)
			require.NoError(t, err)
			require.NotNil(t, deployer)
			err = deployer.Deploy(tc.updates, "commit")
			tc.expect(t, err)
		})
	}
}

func TestNewUpdates(t *testing.T) {
	tcs := []struct {
		name    string
		names   []string
		paths   []string
		values  []string
		expect  []Update
		wantErr string
	}{
		{
			// The xmtpd call site: parallel name/value lists, no paths. This
			// must keep working byte for byte — merging to main force-pushes
			// the v1 tag, so xmtpd picks the change up immediately.
			name:   "no paths zips names to values",
			names:  []string{"xmtpd_image", "prune_image"},
			values: []string{"ghcr.io/xmtp/xmtpd@a", "ghcr.io/xmtp/xmtpd-prune@b"},
			expect: []Update{
				{Name: "xmtpd_image", Value: "ghcr.io/xmtp/xmtpd@a"},
				{Name: "prune_image", Value: "ghcr.io/xmtp/xmtpd-prune@b"},
			},
		},
		{
			name:    "no paths, mismatched lengths",
			names:   []string{"a", "b"},
			values:  []string{"only-one"},
			wantErr: "variable name and value must be the same length",
		},
		{
			// The whole point: one name and one value fan out across a wave of
			// paths, so rolling fifteen heralds is still one flag each.
			name:   "one name and one value broadcast across paths",
			names:  []string{"herald_roster"},
			paths:  []string{"b.image", "c.image", "d.image"},
			values: []string{"img"},
			expect: []Update{
				{Name: "herald_roster", Path: "b.image", Value: "img"},
				{Name: "herald_roster", Path: "c.image", Value: "img"},
				{Name: "herald_roster", Path: "d.image", Value: "img"},
			},
		},
		{
			name:   "per-path values",
			names:  []string{"herald_roster"},
			paths:  []string{"a.image", "b.image"},
			values: []string{"img-a", "img-b"},
			expect: []Update{
				{Name: "herald_roster", Path: "a.image", Value: "img-a"},
				{Name: "herald_roster", Path: "b.image", Value: "img-b"},
			},
		},
		{
			name:    "value count neither one nor per-path",
			names:   []string{"herald_roster"},
			paths:   []string{"a.image", "b.image", "c.image"},
			values:  []string{"img-a", "img-b"},
			wantErr: "got 2 variable values for 3 paths",
		},
		{
			name:    "name count neither one nor per-path",
			names:   []string{"x", "y"},
			paths:   []string{"a.image", "b.image", "c.image"},
			values:  []string{"img"},
			wantErr: "got 2 variable names for 3 paths",
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := NewUpdates(tc.names, tc.paths, tc.values)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.expect, got)
		})
	}
}

// TestRenderVar_Scenarios covers the four shapes a caller can ask for, end to
// end: CLI flags -> updates -> merged, re-rendered HCL. It reads the result back
// out of the rendered output, so each case also asserts the value round-trips.
func TestRenderVar_Scenarios(t *testing.T) {
	tcs := []struct {
		name   string
		paths  []string
		values []string
		expect map[string]string
	}{
		{
			// key -> value
			name:   "one path, one value",
			paths:  []string{"a.image"},
			values: []string{"img"},
			expect: map[string]string{"a": "img", "b": "old-b", "c": "old-c"},
		},
		{
			// key1,key2 -> value1,value2
			name:   "many paths, one value each",
			paths:  []string{"a.image", "b.image"},
			values: []string{"img-a", "img-b"},
			expect: map[string]string{"a": "img-a", "b": "img-b", "c": "old-c"},
		},
		{
			// key1,key2 -> value  (a wave: same image across a subset)
			name:   "many paths, one broadcast value",
			paths:  []string{"a.image", "b.image"},
			values: []string{"img"},
			expect: map[string]string{"a": "img", "b": "img", "c": "old-c"},
		},
		{
			// * -> value  (the whole fleet, without CI knowing its members)
			name:   "wildcard, one broadcast value",
			paths:  []string{"*.image"},
			values: []string{"img"},
			expect: map[string]string{"a": "img", "b": "img", "c": "img"},
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			updates, err := NewUpdates([]string{"herald_roster"}, tc.paths, tc.values)
			require.NoError(t, err)

			d := &Deployer{log: zap.NewNop()}
			out, changed, err := d.renderVar(&tfe.Variable{
				Key:   "herald_roster",
				HCL:   true,
				Value: roster3,
			}, updates)
			require.NoError(t, err)
			require.True(t, changed)

			require.Equal(t, tc.expect, imagesOf(t, out))
			// The disks belong to Terraform, not to us; a roll must never touch them.
			require.Equal(t, map[string]string{
				"a": "herald-dev/herald-a",
				"b": "herald-dev/herald-b",
				"c": "herald-dev/herald-c",
			}, disksOf(t, out))
		})
	}
}

func TestValidatePrefix(t *testing.T) {
	tcs := []struct {
		name    string
		updates []Update
		prefix  string
		wantErr string
	}{
		{
			name:    "no prefix configured",
			updates: []Update{{Name: "v", Value: "anything"}},
		},
		{
			name: "all values carry the prefix",
			updates: []Update{
				{Name: "v", Value: "ghcr.io/xmtp/a"},
				{Name: "w", Value: "ghcr.io/xmtp/b"},
			},
			prefix: "ghcr.io/xmtp/",
		},
		{
			// One name, many values: the shape broadcast mode introduces. The
			// prefix check used to index the name list in lockstep with the
			// value list, so a bad value in any position but the first panicked
			// with an index-out-of-range instead of reporting the bad value.
			name: "one name, many values, a later one is bad",
			updates: []Update{
				{
					Name:  "herald_roster",
					Path:  "a.image",
					Value: "ghcr.io/xmtplabs/herald-lite@sha256:good",
				},
				{Name: "herald_roster", Path: "b.image", Value: "evil.io/bad"},
			},
			prefix:  "ghcr.io/xmtplabs/herald-lite@",
			wantErr: "variable herald_roster:evil.io/bad does not start with required prefix",
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidatePrefix(tc.updates, tc.prefix)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func stringPtr(v string) *string {
	return &v
}
