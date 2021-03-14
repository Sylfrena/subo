package directive

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
	"gopkg.in/yaml.v2"
)

// InputTypeRequest and others represent consts for Directives
const (
	InputTypeRequest = "request"
)

// NamespaceDefault and others represent conts for namespaces
const (
	NamespaceDefault = "default"
)

// Directive describes a set of functions and a set of handlers
// that take an input, and compose a set of functions to handle it
type Directive struct {
	Identifier  string     `yaml:"identifier"`
	AppVersion  string     `yaml:"appVersion"`
	AtmoVersion string     `yaml:"atmoVersion"`
	Runnables   []Runnable `yaml:"runnables"`
	Handlers    []Handler  `yaml:"handlers,omitempty"`
	Schedules   []Schedule `yaml:"schedules,omitempty"`

	// "fully qualified function names"
	fqfns map[string]string `yaml:"-"`
}

// Handler represents the mapping between an input and a composition of functions
type Handler struct {
	Input    Input        `yaml:"input,inline"`
	Steps    []Executable `yaml:"steps"`
	Response string       `yaml:"response,omitempty"`
}

// Schedule represents the mapping between an input and a composition of functions
type Schedule struct {
	Name  string            `yaml:"name"`
	Every ScheduleEvery     `yaml:"every"`
	State map[string]string `yaml:"state,omitempty"`
	Steps []Executable      `yaml:"steps"`
}

// ScheduleEvery represents the 'every' value for a schedule
type ScheduleEvery struct {
	Seconds int `yaml:"seconds,omitempty"`
	Minutes int `yaml:"minutes,omitempty"`
	Hours   int `yaml:"hours,omitempty"`
	Days    int `yaml:"days,omitempty"`
}

// Input represents an input source
type Input struct {
	Type     string
	Method   string
	Resource string
}

// Executable represents an executable step in a handler
type Executable struct {
	CallableFn `yaml:"callableFn,inline"`
	Group      []CallableFn `yaml:"group,omitempty"`
	ForEach    *ForEach     `yaml:"forEach,omitempty"`
}

// CallableFn is a fn along with its "variable name" and "args"
type CallableFn struct {
	Fn           string   `yaml:"fn,omitempty"`
	As           string   `yaml:"as,omitempty"`
	With         []string `yaml:"with,omitempty"`
	OnErr        *FnOnErr `yaml:"onErr,omitempty"`
	DesiredState []Alias  `yaml:"-"`
}

// FnOnErr describes how to handle an error from a function call
type FnOnErr struct {
	Code  map[int]string `yaml:"code,omitempty"`
	Any   string         `yaml:"any,omitempty"`
	Other string         `yaml:"other,omitempty"`
}

type ForEach struct {
	In string     `yaml:"in"`
	Fn CallableFn `yaml:"fn"`
	As string     `yaml:"as"`
}

// Alias is the parsed version of an entry in the `With` array from a CallableFn
// If you do user: activeUser, then activeUser is the state key and user
// is the key that gets put into the function's state (i.e. the alias)
type Alias struct {
	Key   string
	Alias string
}

// Marshal outputs the YAML bytes of the Directive
func (d *Directive) Marshal() ([]byte, error) {
	return yaml.Marshal(d)
}

// Unmarshal unmarshals YAML bytes into a Directive struct
// it also calculates a map of FQFNs for later use
func (d *Directive) Unmarshal(in []byte) error {
	return yaml.Unmarshal(in, d)
}

// FQFN returns the FQFN for a given function in the directive
func (d *Directive) FQFN(fn string) (string, error) {
	if d.fqfns == nil {
		d.calculateFQFNs()
	}

	fqfn, exists := d.fqfns[fn]
	if !exists {
		return "", fmt.Errorf("fn %s does not exist", fn)
	}

	return fqfn, nil
}

// Validate validates a directive
func (d *Directive) Validate() error {
	problems := &problems{}

	if d.Identifier == "" {
		problems.add(errors.New("identifier is missing"))
	}

	if !semver.IsValid(d.AppVersion) {
		problems.add(errors.New("app version is not a valid semantic version"))
	}

	if !semver.IsValid(d.AtmoVersion) {
		problems.add(errors.New("atmo version is not a valid semantic version"))
	}

	if len(d.Runnables) < 1 {
		problems.add(errors.New("no functions listed"))
	}

	fns := map[string]bool{}

	for i, f := range d.Runnables {
		namespaced := fmt.Sprintf("%s#%s", f.Namespace, f.Name)

		if _, exists := fns[namespaced]; exists {
			problems.add(fmt.Errorf("duplicate fn %s found", namespaced))
			continue
		}

		if _, exists := fns[f.Name]; exists {
			problems.add(fmt.Errorf("duplicate fn %s found", namespaced))
			continue
		}

		if f.Name == "" {
			problems.add(fmt.Errorf("function at position %d missing name", i))
			continue
		}
		if f.Namespace == "" {
			problems.add(fmt.Errorf("function at position %d missing namespace", i))
		}

		// if the fn is in the default namespace, let it exist "naked" and namespaced
		if f.Namespace == NamespaceDefault {
			fns[f.Name] = true
			fns[namespaced] = true
		} else {
			fns[namespaced] = true
		}
	}

	for _, h := range d.Handlers {
		if h.Input.Type == "" {
			problems.add(fmt.Errorf("handler for resource %s missing type", h.Input.Resource))
		}

		if h.Input.Resource == "" {
			problems.add(fmt.Errorf("handler for resource %s missing resource", h.Input.Resource))
		}

		if h.Input.Type == InputTypeRequest && h.Input.Method == "" {
			problems.add(fmt.Errorf("handler for resource %s is of type request, but does not specify a method", h.Input.Resource))
		}

		if len(h.Steps) == 0 {
			problems.add(fmt.Errorf("handler for resource %s missing steps", h.Input.Resource))
			continue
		}

		name := fmt.Sprintf("%s %s", h.Input.Method, h.Input.Resource)
		fullState := validateSteps(executableTypeHandler, name, h.Steps, map[string]bool{}, fns, problems)

		lastStep := h.Steps[len(h.Steps)-1]
		if h.Response == "" && lastStep.IsGroup() {
			problems.add(fmt.Errorf("handler for %s has group as last step but does not include 'response' field", name))
		} else if h.Response != "" {
			if _, exists := fullState[h.Response]; !exists {
				problems.add(fmt.Errorf("handler for %s lists response state key that does not exist: %s", name, h.Response))
			}
		}
	}

	for i, s := range d.Schedules {
		if s.Name == "" {
			problems.add(fmt.Errorf("schedule at position %d has no name", i))
			continue
		}

		if len(s.Steps) == 0 {
			problems.add(fmt.Errorf("schedule %s missing steps", s.Name))
			continue
		}

		if s.Every.Seconds == 0 && s.Every.Minutes == 0 && s.Every.Hours == 0 && s.Every.Days == 0 {
			problems.add(fmt.Errorf("schedule %s has no 'every' values", s.Name))
		}

		// user can provide an 'initial state' via the schedule.State field, so let's prime the state with it.
		initialState := map[string]bool{}
		for k := range s.State {
			initialState[k] = true
		}

		validateSteps(executableTypeSchedule, s.Name, s.Steps, initialState, fns, problems)
	}

	return problems.render()
}

type executableType string

const (
	executableTypeHandler  = executableType("handler")
	executableTypeSchedule = executableType("schedule")
)

func validateSteps(exType executableType, name string, steps []Executable, initialState map[string]bool, fns map[string]bool, problems *problems) map[string]bool {
	// keep track of the functions that have run so far at each step
	fullState := initialState

	for j, s := range steps {
		fnsToAdd := []string{}

		if !s.IsFn() && !s.IsGroup() && !s.IsForEach() {
			problems.add(fmt.Errorf("step at position %d for %s %s isn't an Fn, Group, or ForEach", j, exType, name))
		}

		validateFn := func(fn CallableFn) {
			if _, exists := fns[fn.Fn]; !exists {
				problems.add(fmt.Errorf("%s for %s lists fn at step %d that does not exist: %s (did you forget a namespace?)", exType, name, j, fn.Fn))
			}

			if _, err := fn.ParseWith(); err != nil {
				problems.add(fmt.Errorf("%s for %s has invalid 'with' value at step %d: %s", exType, name, j, err.Error()))
			}

			for _, d := range fn.DesiredState {
				if _, exists := fullState[d.Key]; !exists {
					problems.add(fmt.Errorf("%s for %s has 'with' value at step %d referencing a key that is not yet available in the handler's state: %s", exType, name, j, d.Key))
				}
			}

			if fn.OnErr != nil {
				// if codes are specificed, 'other' should be used, not 'any'
				if len(fn.OnErr.Code) > 0 && fn.OnErr.Any != "" {
					problems.add(fmt.Errorf("%s for %s has 'onErr.any' value at step %d while specific codes are specified, use 'other' instead", exType, name, j))
				} else if fn.OnErr.Any != "" {
					if fn.OnErr.Any != "continue" && fn.OnErr.Any != "return" {
						problems.add(fmt.Errorf("%s for %s has 'onErr.any' value at step %d with an invalid error directive: %s", exType, name, j, fn.OnErr.Any))
					}
				}

				// if codes are NOT specificed, 'any' should be used, not 'other'
				if len(fn.OnErr.Code) == 0 && fn.OnErr.Other != "" {
					problems.add(fmt.Errorf("%s for %s has 'onErr.other' value at step %d while specific codes are not specified, use 'any' instead", exType, name, j))
				} else if fn.OnErr.Other != "" {
					if fn.OnErr.Other != "continue" && fn.OnErr.Other != "return" {
						problems.add(fmt.Errorf("%s for %s has 'onErr.any' value at step %d with an invalid error directive: %s", exType, name, j, fn.OnErr.Other))
					}
				}

				for code, val := range fn.OnErr.Code {
					if val != "return" && val != "continue" {
						problems.add(fmt.Errorf("%s for %s has 'onErr.code' value at step %d with an invalid error directive for code %d: %s", exType, name, j, code, val))
					}
				}
			}

			key := fn.Fn
			if fn.As != "" {
				key = fn.As
			}

			fnsToAdd = append(fnsToAdd, key)
		}

		if s.IsFn() {
			validateFn(s.CallableFn)
		} else if s.IsGroup() {
			for _, gfn := range s.Group {
				validateFn(gfn)
			}
		} else if s.IsForEach() {
			if s.ForEach.As == "" {
				problems.add(fmt.Errorf("ForEach at position %d for %s %s is missing 'as' value", j, exType, name))
			}

			if s.ForEach.Fn.As != "" || (s.ForEach.Fn.With != nil && len(s.ForEach.Fn.With) > 0) {
				problems.add(fmt.Errorf("ForEach at position %d for %s %s should not have 'fn.as' or 'fn.with' ", j, exType, name))
			}

			validateFn(s.ForEach.Fn)

			// the key for a ForEach is not the actual Fn, it's the ForEach's As value
			// so replace the value that validateFn sets automatically
			fnsToAdd[len(fnsToAdd)-1] = s.ForEach.As
		}

		for _, newFn := range fnsToAdd {
			fullState[newFn] = true
		}
	}

	return fullState
}

func (d *Directive) calculateFQFNs() {
	d.fqfns = map[string]string{}

	for _, fn := range d.Runnables {
		namespaced := fmt.Sprintf("%s#%s", fn.Namespace, fn.Name)

		// if the function is in the default namespace, add it to the map both namespaced and not
		if fn.Namespace == NamespaceDefault {
			d.fqfns[fn.Name] = d.fqfnForFunc(fn.Namespace, fn.Name)
			d.fqfns[namespaced] = d.fqfnForFunc(fn.Namespace, fn.Name)
		} else {
			d.fqfns[namespaced] = d.fqfnForFunc(fn.Namespace, fn.Name)
		}
	}
}

func (d *Directive) fqfnForFunc(namespace, fn string) string {
	return fmt.Sprintf("%s#%s@%s", namespace, fn, d.AppVersion)
}

// NumberOfSeconds calculates the total time in seconds for the schedule's 'every' value
func (s *Schedule) NumberOfSeconds() int {
	seconds := s.Every.Seconds
	minutes := 60 * s.Every.Minutes
	hours := 60 * 60 * s.Every.Hours
	days := 60 * 60 * 24 * s.Every.Days

	return seconds + minutes + hours + days
}

// IsGroup returns true if the executable is a group
func (e *Executable) IsGroup() bool {
	return e.Fn == "" && e.Group != nil && len(e.Group) > 0 && e.ForEach == nil
}

// IsFn returns true if the executable is a group
func (e *Executable) IsFn() bool {
	return e.Fn != "" && e.Group == nil && e.ForEach == nil
}

// IsForEach returns true if the exectuable is a ForEach
func (e *Executable) IsForEach() bool {
	return e.ForEach != nil && e.Fn == "" && e.Group == nil
}

// ParseWith parses the fn's 'with' clause and returns the desired state
func (c *CallableFn) ParseWith() ([]Alias, error) {
	if c.DesiredState != nil && len(c.DesiredState) > 0 {
		return c.DesiredState, nil
	}

	c.DesiredState = make([]Alias, len(c.With))

	for i, w := range c.With {
		parts := strings.Split(w, ": ")
		if len(parts) != 2 {
			return nil, fmt.Errorf("with value has wrong format: parsed %d parts seperated by : , expected 2", len(parts))
		}

		c.DesiredState[i] = Alias{Alias: parts[0], Key: parts[1]}
	}

	return c.DesiredState, nil
}

type problems []error

func (p *problems) add(err error) {
	*p = append(*p, err)
}

func (p *problems) render() error {
	if len(*p) == 0 {
		return nil
	}

	text := fmt.Sprintf("found %d problems:", len(*p))

	for _, err := range *p {
		text += fmt.Sprintf("\n\t%s", err.Error())
	}

	return errors.New(text)
}