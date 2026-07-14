package deployer

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-tfe"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	defaultWaitTimeout = 15 * time.Minute
	defaultWaitDelay   = 3 * time.Second

	// Terraform Cloud defaults to 20 variables per page. Workspaces routinely
	// carry more than that, and a variable that falls onto page 2 would look
	// like it does not exist.
	variablePageSize = 100
)

// Update is a single write against a workspace variable.
//
// Path is optional. Empty replaces the variable's whole value (a plain string
// variable, e.g. a docker image). Non-empty addresses a string inside an
// HCL-typed variable by dotted path (e.g. "a.image" within a map), leaving the
// rest of the value intact. A "*" segment fans the write out across every key
// at that level, so a fleet can be rolled without CI knowing its members.
type Update struct {
	Name  string
	Path  string
	Value string
}

// NewUpdates zips the parallel --variable-name / --variable-path /
// --variable-value lists into updates.
//
// With no paths, names and values line up one-to-one — the original behavior.
//
// With paths, the list length is set by the paths, and a single name or a
// single value broadcasts across all of them. That is what keeps a fleet-wide
// roll to one flag each:
//
//	--variable-name herald_roster --variable-path 'b.image,c.image' --variable-value IMG
func NewUpdates(names, paths, values []string) ([]Update, error) {
	if len(names) == 0 {
		return nil, errors.New("at least one variable name is required")
	}
	if len(values) == 0 {
		return nil, errors.New("at least one variable value is required")
	}

	if len(paths) == 0 {
		if len(names) != len(values) {
			return nil, errors.New("variable name and value must be the same length")
		}

		updates := make([]Update, len(names))
		for i := range names {
			updates[i] = Update{Name: names[i], Value: values[i]}
		}
		return updates, nil
	}

	if len(names) != 1 && len(names) != len(paths) {
		return nil, fmt.Errorf(
			"got %d variable names for %d paths: pass one name to share across every path, or one per path",
			len(names),
			len(paths),
		)
	}
	if len(values) != 1 && len(values) != len(paths) {
		return nil, fmt.Errorf(
			"got %d variable values for %d paths: pass one value to share across every path, or one per path",
			len(values),
			len(paths),
		)
	}

	updates := make([]Update, len(paths))
	for i := range paths {
		updates[i] = Update{
			Name:  at(names, i),
			Path:  paths[i],
			Value: at(values, i),
		}
	}
	return updates, nil
}

// at indexes a list that is either the full length or a single broadcast value.
func at(list []string, i int) string {
	if len(list) == 1 {
		return list[0]
	}
	return list[i]
}

// ValidatePrefix rejects any update whose value does not carry the required
// prefix, guarding against a malformed image being written to the workspace.
//
// It runs over the updates rather than the raw flag lists: one variable name
// can map to many values now, so nothing can walk names and values in lockstep.
func ValidatePrefix(updates []Update, prefix string) error {
	if prefix == "" {
		return nil
	}

	for _, u := range updates {
		if !strings.HasPrefix(u.Value, prefix) {
			return fmt.Errorf(
				"variable %s:%s does not start with required prefix",
				u.Name,
				u.Value,
			)
		}
	}

	return nil
}

type Config struct {
	Organization string
	Workspace    string

	WaitTimeout time.Duration
	WaitDelay   time.Duration
}

type Deployer struct {
	config *Config
	ctx    context.Context
	log    *zap.Logger
	tfe    *tfe.Client
	wsp    *tfe.Workspace
}

func NewDeployer(
	ctx context.Context,
	log *zap.Logger,
	tfc *tfe.Client,
	config *Config,
) (*Deployer, error) {
	wsp, err := tfc.Workspaces.Read(ctx, config.Organization, config.Workspace)
	if err != nil {
		return nil, errors.Wrap(
			err,
			fmt.Sprintf("getting workspace %s/%s", config.Organization, config.Workspace),
		)
	}

	if config.WaitTimeout == 0 {
		config.WaitTimeout = defaultWaitTimeout
	}
	if config.WaitDelay == 0 {
		config.WaitDelay = defaultWaitDelay
	}

	return &Deployer{
		config: config,
		ctx:    ctx,
		log:    log,
		tfe:    tfc,
		wsp:    wsp,
	}, nil
}

// Deploy applies every update, then creates a single run for all of them.
//
// One run, not one per update: a run is a full plan+apply of the workspace, so
// rolling fifteen services in one wave must not mean fifteen applies.
func (d *Deployer) Deploy(updates []Update, msg string) error {
	if len(updates) == 0 {
		return errors.New("no updates to apply")
	}

	vars, err := d.listVars()
	if err != nil {
		return errors.Wrap(err, "listing workspace vars")
	}

	changed := false
	for _, name := range updateNames(updates) {
		didChange, err := d.updateVar(vars, name, updatesFor(updates, name))
		if err != nil {
			return errors.Wrapf(err, "updating variable %s", name)
		}
		changed = changed || didChange
	}

	// An auto-applied run applies the whole workspace, so an image bump that is
	// already live must not drag unrelated pending changes out with it.
	if !changed {
		d.log.Info("no variable changed, skipping run")
		return nil
	}

	run, err := d.tfe.Runs.Create(d.ctx, tfe.RunCreateOptions{
		Message:   &msg,
		Workspace: d.wsp,
		AutoApply: boolPtr(true),
	})
	if err != nil {
		return errors.Wrap(err, "creating run")
	}

	err = d.runWait(run.ID)
	if err != nil {
		return errors.Wrap(err, "waiting on run")
	}

	return nil
}

// updateVar writes every update targeting one variable in a single read-modify-
// write, and reports whether the value actually moved.
func (d *Deployer) updateVar(vars []*tfe.Variable, name string, updates []Update) (bool, error) {
	// Replace-only, never upsert: a variable we did not find is a typo or a
	// workspace that was never set up, not an invitation to create one.
	v := findVar(vars, name)
	if v == nil {
		return false, fmt.Errorf("variable %s not found", name)
	}

	value, changed, err := d.renderVar(v, updates)
	if err != nil {
		return false, err
	}

	if !changed {
		d.log.Info("variable already up to date", zap.String("variable", name))
		return false, nil
	}

	_, err = d.tfe.Variables.Update(
		d.ctx,
		d.wsp.ID,
		v.ID,
		tfe.VariableUpdateOptions{Value: &value},
	)
	if err != nil {
		return false, err
	}

	return true, nil
}

// renderVar computes a variable's new value from the updates targeting it, and
// reports whether that value actually moved.
func (d *Deployer) renderVar(v *tfe.Variable, updates []Update) (string, bool, error) {
	if whole := wholeValueUpdate(updates); whole != nil {
		if len(updates) > 1 {
			return "", false, fmt.Errorf(
				"variable %s has both a whole-value update and a path update; pick one",
				v.Key,
			)
		}
		return whole.Value, whole.Value != v.Value, nil
	}

	if !v.HCL {
		return "", false, fmt.Errorf(
			"variable %s is not HCL-typed, so it has no paths to address",
			v.Key,
		)
	}

	current, err := parseHCLValue(v.Key, v.Value)
	if err != nil {
		return "", false, err
	}

	val := current
	for _, u := range updates {
		segments, err := splitPath(u.Path)
		if err != nil {
			return "", false, err
		}

		next, written, err := setPath(val, segments, u.Value)
		if err != nil {
			return "", false, errors.Wrapf(err, "setting %s.%s", v.Key, u.Path)
		}
		val = next

		// A "*" hides how many things it touched; say so out loud, so a wave
		// that matched more or fewer keys than intended is visible in the log
		// rather than inferred from the Terraform diff.
		d.log.Info(
			"set",
			zap.String("variable", v.Key),
			zap.String("path", u.Path),
			zap.String("value", u.Value),
			zap.Strings("matched", written),
		)
	}

	// Compare the parsed values, not the rendered text. Writing the variable
	// canonicalizes its formatting, so a roster a human wrote with different
	// whitespace — or in the JSON dialect, which is also valid HCL — would
	// re-render differently even when every image is unchanged. Comparing text
	// would read that as a change and fire an auto-applied run, which applies
	// the whole workspace, for a deploy that moved nothing.
	return renderHCLValue(val), !val.RawEquals(current), nil
}

// listVars pages through every variable on the workspace.
func (d *Deployer) listVars() ([]*tfe.Variable, error) {
	var out []*tfe.Variable

	page := 1
	for {
		list, err := d.tfe.Variables.List(d.ctx, d.wsp.ID, &tfe.VariableListOptions{
			ListOptions: tfe.ListOptions{
				PageNumber: page,
				PageSize:   variablePageSize,
			},
		})
		if err != nil {
			return nil, err
		}

		out = append(out, list.Items...)

		// Pagination is embedded as a pointer and is absent on a single-page
		// response.
		if list.Pagination == nil || list.NextPage <= page {
			return out, nil
		}
		page = list.NextPage
	}
}

func (d *Deployer) runWait(runID string) error {
	started := time.Now()
	for {
		run, err := d.tfe.Runs.Read(d.ctx, runID)
		if err != nil {
			return errors.Wrap(err, "reading run")
		}

		switch run.Status {
		case tfe.RunApplied:
			d.log.Info("success", zap.String("status", string(run.Status)))
			return nil
		case tfe.RunPlannedAndFinished:
			d.log.Info("success", zap.String("status", string(run.Status)))
			return nil
		case tfe.RunErrored:
			d.log.Info("failed", zap.String("status", string(run.Status)))
			return fmt.Errorf("run failed with status %q", run.Status)
		case tfe.RunDiscarded:
			d.log.Info("canceled", zap.String("status", string(run.Status)))
			return fmt.Errorf("run canceled with status %q", run.Status)
		case tfe.RunCanceled:
			d.log.Info("canceled", zap.String("status", string(run.Status)))
			return fmt.Errorf("run canceled with status %q", run.Status)
		default:
			d.log.Info("waiting", zap.String("status", string(run.Status)))
		}

		if time.Since(started) > d.config.WaitTimeout {
			return fmt.Errorf("timeout waiting for run")
		}

		time.Sleep(d.config.WaitDelay)
	}
}

// updateNames lists the variables the updates target, in first-seen order.
func updateNames(updates []Update) []string {
	var names []string
	seen := make(map[string]bool, len(updates))
	for _, u := range updates {
		if !seen[u.Name] {
			seen[u.Name] = true
			names = append(names, u.Name)
		}
	}
	return names
}

func updatesFor(updates []Update, name string) []Update {
	var out []Update
	for _, u := range updates {
		if u.Name == name {
			out = append(out, u)
		}
	}
	return out
}

func wholeValueUpdate(updates []Update) *Update {
	for i, u := range updates {
		if strings.TrimSpace(u.Path) == "" {
			return &updates[i]
		}
	}
	return nil
}

func findVar(vars []*tfe.Variable, name string) *tfe.Variable {
	for _, v := range vars {
		if v.Key == name {
			return v
		}
	}
	return nil
}

func boolPtr(v bool) *bool {
	return &v
}
