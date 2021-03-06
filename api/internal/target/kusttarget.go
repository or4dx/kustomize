// Copyright 2019 The Kubernetes Authors.
// SPDX-License-Identifier: Apache-2.0

package target

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"sigs.k8s.io/kustomize/api/builtins"
	"sigs.k8s.io/kustomize/api/ifc"
	"sigs.k8s.io/kustomize/api/internal/accumulator"
	"sigs.k8s.io/kustomize/api/internal/plugins/builtinconfig"
	"sigs.k8s.io/kustomize/api/internal/plugins/builtinhelpers"
	"sigs.k8s.io/kustomize/api/internal/plugins/loader"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/yaml"
)

// KustTarget encapsulates the entirety of a kustomization build.
type KustTarget struct {
	kustomization *types.Kustomization
	ldr           ifc.Loader
	validator     ifc.Validator
	rFactory      *resmap.Factory
	pLdr          *loader.Loader
}

// NewKustTarget returns a new instance of KustTarget.
func NewKustTarget(
	ldr ifc.Loader,
	validator ifc.Validator,
	rFactory *resmap.Factory,
	pLdr *loader.Loader) *KustTarget {
	return &KustTarget{
		ldr:       ldr,
		validator: validator,
		rFactory:  rFactory,
		pLdr:      pLdr,
	}
}

// Load attempts to load the target's kustomization file.
func (kt *KustTarget) Load() error {
	content, err := loadKustFile(kt.ldr)
	if err != nil {
		return err
	}
	content, err = types.FixKustomizationPreUnmarshalling(content)
	if err != nil {
		return err
	}
	var k types.Kustomization
	err = k.Unmarshal(content)
	if err != nil {
		return err
	}
	k.FixKustomizationPostUnmarshalling()
	errs := k.EnforceFields()
	if len(errs) > 0 {
		return fmt.Errorf(
			"Failed to read kustomization file under %s:\n"+
				strings.Join(errs, "\n"), kt.ldr.Root())
	}
	kt.kustomization = &k
	return nil
}

// Kustomization returns a copy of the immutable, internal kustomization object.
func (kt *KustTarget) Kustomization() types.Kustomization {
	var result types.Kustomization
	b, _ := json.Marshal(*kt.kustomization)
	json.Unmarshal(b, &result)
	return result
}

func loadKustFile(ldr ifc.Loader) ([]byte, error) {
	var content []byte
	match := 0
	for _, kf := range konfig.RecognizedKustomizationFileNames() {
		c, err := ldr.Load(kf)
		if err == nil {
			match += 1
			content = c
		}
	}
	switch match {
	case 0:
		return nil, NewErrMissingKustomization(ldr.Root())
	case 1:
		return content, nil
	default:
		return nil, fmt.Errorf(
			"Found multiple kustomization files under: %s\n", ldr.Root())
	}
}

// MakeCustomizedResMap creates a fully customized ResMap
// per the instructions contained in its kustomization instance.
func (kt *KustTarget) MakeCustomizedResMap() (resmap.ResMap, error) {
	return kt.makeCustomizedResMap()
}

func (kt *KustTarget) makeCustomizedResMap() (resmap.ResMap, error) {
	ra, err := kt.AccumulateTarget()
	if err != nil {
		return nil, err
	}

	// The following steps must be done last, not as part of
	// the recursion implicit in AccumulateTarget.

	err = kt.addHashesToNames(ra)
	if err != nil {
		return nil, err
	}

	// Given that names have changed (prefixs/suffixes added),
	// fix all the back references to those names.
	err = ra.FixBackReferences()
	if err != nil {
		return nil, err
	}

	// With all the back references fixed, it's OK to resolve Vars.
	err = ra.ResolveVars()
	if err != nil {
		return nil, err
	}

	return ra.ResMap(), nil
}

func (kt *KustTarget) addHashesToNames(
	ra *accumulator.ResAccumulator) error {
	p := builtins.NewHashTransformerPlugin()
	err := kt.configureBuiltinPlugin(p, nil, builtinhelpers.HashTransformer)
	if err != nil {
		return err
	}
	return ra.Transform(p)
}

// AccumulateTarget returns a new ResAccumulator,
// holding customized resources and the data/rules used
// to do so.  The name back references and vars are
// not yet fixed.
func (kt *KustTarget) AccumulateTarget() (
	ra *accumulator.ResAccumulator, err error) {
	return kt.accumulateTarget(accumulator.MakeEmptyAccumulator())
}

// ra should be empty when this KustTarget is a Kustomization, or the ra of the parent if this KustTarget is a Component
// (or empty if the Component does not have a parent).
func (kt *KustTarget) accumulateTarget(ra *accumulator.ResAccumulator) (
	resRa *accumulator.ResAccumulator, err error) {
	ra, err = kt.accumulateResources(ra, kt.kustomization.Resources)
	if err != nil {
		return nil, errors.Wrap(err, "accumulating resources")
	}
	ra, err = kt.accumulateComponents(ra, kt.kustomization.Components)
	if err != nil {
		return nil, errors.Wrap(err, "accumulating components")
	}
	tConfig, err := builtinconfig.MakeTransformerConfig(
		kt.ldr, kt.kustomization.Configurations)
	if err != nil {
		return nil, err
	}
	err = ra.MergeConfig(tConfig)
	if err != nil {
		return nil, errors.Wrapf(
			err, "merging config %v", tConfig)
	}
	crdTc, err := accumulator.LoadConfigFromCRDs(kt.ldr, kt.kustomization.Crds)
	if err != nil {
		return nil, errors.Wrapf(
			err, "loading CRDs %v", kt.kustomization.Crds)
	}
	err = ra.MergeConfig(crdTc)
	if err != nil {
		return nil, errors.Wrapf(
			err, "merging CRDs %v", crdTc)
	}
	err = kt.runGenerators(ra)
	if err != nil {
		return nil, err
	}
	err = kt.runTransformers(ra)
	if err != nil {
		return nil, err
	}
	err = kt.runValidators(ra)
	if err != nil {
		return nil, err
	}
	err = ra.MergeVars(kt.kustomization.Vars)
	if err != nil {
		return nil, errors.Wrapf(
			err, "merging vars %v", kt.kustomization.Vars)
	}
	return ra, nil
}

func (kt *KustTarget) runGenerators(
	ra *accumulator.ResAccumulator) error {
	var generators []resmap.Generator
	gs, err := kt.configureBuiltinGenerators()
	if err != nil {
		return err
	}
	generators = append(generators, gs...)
	gs, err = kt.configureExternalGenerators()
	if err != nil {
		return errors.Wrap(err, "loading generator plugins")
	}
	generators = append(generators, gs...)
	for _, g := range generators {
		resMap, err := g.Generate()
		if err != nil {
			return err
		}
		err = ra.AbsorbAll(resMap)
		if err != nil {
			return errors.Wrapf(err, "merging from generator %v", g)
		}
	}
	return nil
}

func (kt *KustTarget) configureExternalGenerators() ([]resmap.Generator, error) {
	ra := accumulator.MakeEmptyAccumulator()
	ra, err := kt.accumulateResources(ra, kt.kustomization.Generators)
	if err != nil {
		return nil, err
	}
	return kt.pLdr.LoadGenerators(kt.ldr, kt.validator, ra.ResMap())
}

func (kt *KustTarget) runTransformers(ra *accumulator.ResAccumulator) error {
	var r []resmap.Transformer
	tConfig := ra.GetTransformerConfig()
	lts, err := kt.configureBuiltinTransformers(tConfig)
	if err != nil {
		return err
	}
	r = append(r, lts...)
	lts, err = kt.configureExternalTransformers(kt.kustomization.Transformers)
	if err != nil {
		return err
	}
	r = append(r, lts...)
	return ra.Transform(newMultiTransformer(r))
}

func (kt *KustTarget) configureExternalTransformers(transformers []string) ([]resmap.Transformer, error) {
	ra := accumulator.MakeEmptyAccumulator()
	ra, err := kt.accumulateResources(ra, transformers)

	if err != nil {
		return nil, err
	}
	return kt.pLdr.LoadTransformers(kt.ldr, kt.validator, ra.ResMap())
}

func (kt *KustTarget) runValidators(ra *accumulator.ResAccumulator) error {
	validators, err := kt.configureExternalTransformers(kt.kustomization.Validators)
	if err != nil {
		return err
	}
	for _, v := range validators {
		// Validators shouldn't modify the resource map
		orignal := ra.ResMap().DeepCopy()
		err = v.Transform(ra.ResMap())
		if err != nil {
			return err
		}
		new := ra.ResMap().DeepCopy()
		kt.removeValidatedByLabel(new)
		if err = orignal.ErrorIfNotEqualSets(new); err != nil {
			return fmt.Errorf("validator shouldn't modify the resource map: %v", err)
		}
	}
	return nil
}

func (kt *KustTarget) removeValidatedByLabel(rm resmap.ResMap) {
	resources := rm.Resources()
	for _, r := range resources {
		labels := r.GetLabels()
		if _, found := labels[konfig.ValidatedByLabelKey]; !found {
			continue
		}
		delete(labels, konfig.ValidatedByLabelKey)
		if len(labels) == 0 {
			r.SetLabels(nil)
		} else {
			r.SetLabels(labels)
		}
	}
}

// accumulateResources fills the given resourceAccumulator
// with resources read from the given list of paths.
func (kt *KustTarget) accumulateResources(
	ra *accumulator.ResAccumulator, paths []string) (*accumulator.ResAccumulator, error) {
	for _, path := range paths {
		// try loading resource as file then as base (directory or git repository)
		if errF := kt.accumulateFile(ra, path); errF != nil {
			ldr, errL := kt.ldr.New(path)
			if errL != nil {
				return nil, fmt.Errorf("accumulateFile %q, loader.New %q", errF, errL)
			}
			var errD error
			ra, errD = kt.accumulateDirectory(ra, ldr, false)
			if errD != nil {
				return nil, fmt.Errorf("accumulateFile %q, accumulateDirector: %q", errF, errD)
			}
		}
	}
	return ra, nil
}

// accumulateResources fills the given resourceAccumulator
// with resources read from the given list of paths.
func (kt *KustTarget) accumulateComponents(
	ra *accumulator.ResAccumulator, paths []string) (*accumulator.ResAccumulator, error) {
	for _, path := range paths {
		// Components always refer to directories
		ldr, errL := kt.ldr.New(path)
		if errL != nil {
			return nil, fmt.Errorf("loader.New %q", errL)
		}
		var errD error
		ra, errD = kt.accumulateDirectory(ra, ldr, true)
		if errD != nil {
			return nil, fmt.Errorf("accumulateDirectory: %q", errD)
		}
	}
	return ra, nil
}

func (kt *KustTarget) accumulateDirectory(
	ra *accumulator.ResAccumulator, ldr ifc.Loader, isComponent bool) (*accumulator.ResAccumulator, error) {
	defer ldr.Cleanup()
	subKt := NewKustTarget(ldr, kt.validator, kt.rFactory, kt.pLdr)
	err := subKt.Load()
	if err != nil {
		return nil, errors.Wrapf(
			err, "couldn't make target for path '%s'", ldr.Root())
	}
	if isComponent && subKt.kustomization.Kind != types.ComponentKind {
		return nil, fmt.Errorf(
			"expected kind '%s' for path '%s' but got '%s'", types.ComponentKind, ldr.Root(), subKt.kustomization.Kind)
	} else if !isComponent && subKt.kustomization.Kind == types.ComponentKind {
		return nil, fmt.Errorf(
			"expected kind != '%s' for path '%s'", types.ComponentKind, ldr.Root())
	}

	var subRa *accumulator.ResAccumulator
	if isComponent {
		// Components don't create a new accumulator: the kustomization directives are added to the current accumulator
		subRa, err = subKt.accumulateTarget(ra)
		ra = accumulator.MakeEmptyAccumulator()
	} else {
		// Child Kustomizations create a new accumulator which resolves their kustomization directives, which will later
		// be merged into the current accumulator.
		subRa, err = subKt.AccumulateTarget()
	}
	if err != nil {
		return nil, errors.Wrapf(
			err, "recursed accumulation of path '%s'", ldr.Root())
	}
	err = ra.MergeAccumulator(subRa)
	if err != nil {
		return nil, errors.Wrapf(
			err, "recursed merging from path '%s'", ldr.Root())
	}
	return ra, nil
}

func (kt *KustTarget) accumulateFile(
	ra *accumulator.ResAccumulator, path string) error {
	resources, err := kt.rFactory.FromFile(kt.ldr, path)
	if err != nil {
		return errors.Wrapf(err, "accumulating resources from '%s'", path)
	}
	err = ra.AppendAll(resources)
	if err != nil {
		return errors.Wrapf(err, "merging resources from '%s'", path)
	}
	return nil
}

func (kt *KustTarget) configureBuiltinPlugin(
	p resmap.Configurable, c interface{}, bpt builtinhelpers.BuiltinPluginType) (err error) {
	var y []byte
	if c != nil {
		y, err = yaml.Marshal(c)
		if err != nil {
			return errors.Wrapf(
				err, "builtin %s marshal", bpt)
		}
	}
	err = p.Config(resmap.NewPluginHelpers(kt.ldr, kt.validator, kt.rFactory), y)
	if err != nil {
		return errors.Wrapf(
			err, "trouble configuring builtin %s with config: `\n%s`", bpt, string(y))
	}
	return nil
}
