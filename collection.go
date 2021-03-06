package ebpf

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/pkg/errors"
)

// CollectionOptions control loading a collection into the kernel.
type CollectionOptions struct {
	Programs ProgramOptions
}

// CollectionSpec describes a collection.
type CollectionSpec struct {
	Maps     map[string]*MapSpec
	Programs map[string]*ProgramSpec
}

// Copy returns a recursive copy of the spec.
func (cs *CollectionSpec) Copy() *CollectionSpec {
	if cs == nil {
		return nil
	}

	cpy := CollectionSpec{
		Maps:     make(map[string]*MapSpec, len(cs.Maps)),
		Programs: make(map[string]*ProgramSpec, len(cs.Programs)),
	}

	for name, spec := range cs.Maps {
		cpy.Maps[name] = spec.Copy()
	}

	for name, spec := range cs.Programs {
		cpy.Programs[name] = spec.Copy()
	}

	return &cpy
}

// LoadCollectionSpec parse an object file and convert it to a collection
func LoadCollectionSpec(file string) (*CollectionSpec, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	return LoadCollectionSpecFromReader(f)
}

// Collection is a collection of Programs and Maps associated
// with their symbols
type Collection struct {
	Programs map[string]*Program
	Maps     map[string]*Map
}

// NewCollection creates a Collection from a specification.
//
// Only maps referenced by at least one of the programs are initialized.
func NewCollection(spec *CollectionSpec) (*Collection, error) {
	return NewCollectionWithOptions(spec, CollectionOptions{})
}

// NewCollectionWithOptions creates a Collection from a specification.
//
// Only maps referenced by at least one of the programs are initialized.
func NewCollectionWithOptions(spec *CollectionSpec, opts CollectionOptions) (*Collection, error) {
	maps := make(map[string]*Map)
	for mapName, mapSpec := range spec.Maps {
		m, err := NewMap(mapSpec)
		if err != nil {
			return nil, errors.Wrapf(err, "map %s", mapName)
		}
		maps[mapName] = m
	}

	progs := make(map[string]*Program)
	for progName, origProgSpec := range spec.Programs {
		progSpec := origProgSpec.Copy()
		editor := Edit(&progSpec.Instructions)

		// Rewrite any Symbol which is a valid Map.
		for sym := range editor.ReferenceOffsets {
			m, ok := maps[sym]
			if !ok {
				continue
			}

			// don't overwrite maps already rewritten, users can rewrite programs in the spec themselves
			if err := editor.rewriteMap(sym, m, false); err != nil {
				return nil, errors.Wrapf(err, "program %s", progName)
			}
		}

		prog, err := NewProgramWithOptions(progSpec, opts.Programs)
		if err != nil {
			return nil, errors.Wrapf(err, "program %s", progName)
		}
		progs[progSpec.SectionName] = prog
	}

	return &Collection{
		progs,
		maps,
	}, nil
}

// LoadCollection parses an object file and converts it to a collection.
func LoadCollection(file string) (*Collection, error) {
	spec, err := LoadCollectionSpec(file)
	if err != nil {
		return nil, err
	}
	return NewCollection(spec)
}

// EnableKprobes enables all kprobes/kretprobes included in the collection.
//
// For kretprobes, you can configure the maximum number of instances
// of the function that can be probed simultaneously with maxactive.
// If maxactive is 0 it will be set to the default value: if CONFIG_PREEMPT is
// enabled, this is max(10, 2*NR_CPUS); otherwise, it is NR_CPUS.
// For kprobes, maxactive is ignored.
func (coll *Collection) EnableKprobes(maxactive int) error {
	for name, prog := range coll.Programs {
		if prog.IsKProbe() || prog.IsKRetProbe() {
			if err := coll.EnableKprobe(name, maxactive); err != nil {
				return errors.Wrapf(err, "couldn't enable kprobe %s", name)
			}
		}
	}
	return nil
}

// EnableKprobe enables the kprobe selected by its section name.
//
// For kretprobes, you can configure the maximum number of instances
// of the function that can be probed simultaneously with maxactive.
// If maxactive is 0 it will be set to the default value: if CONFIG_PREEMPT is
// enabled, this is max(10, 2*NR_CPUS); otherwise, it is NR_CPUS.
// For kprobes, maxactive is ignored.
func (coll *Collection) EnableKprobe(secName string, maxactive int) error {
	// Check if section exists
	prog, ok := coll.Programs[secName]
	if !ok {
		return errors.Wrapf(
			errors.New("section not found"),
			"couldn't enable kprobe %s",
			secName,
		)
	}
	if prog.IsKProbe() || prog.IsKRetProbe() {
		return prog.EnableKprobe(maxactive)
	}
	return errors.Wrapf(
		errors.New("not a kprobe"),
		"couldn't enable program %s",
		secName,
	)
}

// EnableTracepoints enables all tracepoints included in the collection.
func (coll *Collection) EnableTracepoints() error {
	for name, prog := range coll.Programs {
		if prog.ProgramSpec.Type == TracePoint {
			if err := coll.EnableTracepoint(name); err != nil {
				return errors.Wrapf(err, "couldn't enable tracepoint %s", name)
			}
		}
	}
	return nil
}

// EnableTracepoint enables the tracepoint selected by its section name.
func (coll *Collection) EnableTracepoint(secName string) error {
	// Check if section exists
	prog, ok := coll.Programs[secName]
	if !ok {
		return errors.Wrapf(
			errors.New("section not found"),
			"couldn't enable tracepoint %s",
			secName,
		)
	}
	if prog.ProgramSpec.Type == TracePoint {
		return prog.EnableTracepoint()
	}
	return errors.Wrapf(
		errors.New("not a tracepoint"),
		"couldn't enable program %s",
		secName,
	)
}

// AttachCgroupProgram attaches a program to a cgroup
func (coll *Collection) AttachCgroupProgram(secName string, cgroupPath string) error {
	prog, ok := coll.Programs[secName]
	if !ok {
		return errors.Wrapf(
			errors.New("section not found"),
			"couldn't attach program %s",
			secName,
		)
	}
	if prog.IsCgroupProgram() {
		return prog.AttachCgroup(cgroupPath)
	}
	return errors.Wrapf(
		errors.New("not a cgroup program"),
		"couldn't attach program %s",
		secName,
	)
}

// Close frees all maps and programs associated with the collection.
//
// The collection mustn't be used afterwards.
func (coll *Collection) Close() []error {
	errs := []error{}
	for secName, prog := range coll.Programs {
		if errTmp := prog.Close(); errTmp != nil {
			errs = append(errs, errors.Wrapf(errTmp, "couldn't close program %s", secName))
		}
	}
	for secName, m := range coll.Maps {
		if errTmp := m.Close(); errTmp != nil {
			errs = append(errs, errors.Wrapf(errTmp, "couldn't close map %s", secName))
		}
	}
	return errs
}

// DetachMap removes the named map from the Collection.
//
// This means that a later call to Close() will not affect this map.
//
// Returns nil if no map of that name exists.
func (coll *Collection) DetachMap(name string) *Map {
	m := coll.Maps[name]
	delete(coll.Maps, name)
	return m
}

// DetachProgram removes the named program from the Collection.
//
// This means that a later call to Close() will not affect this program.
//
// Returns nil if no program of that name exists.
func (coll *Collection) DetachProgram(name string) *Program {
	p := coll.Programs[name]
	delete(coll.Programs, name)
	return p
}

// Pin persits a Collection beyond the lifetime of the process that created it
//
// This requires bpffs to be mounted above fileName. See http://cilium.readthedocs.io/en/doc-1.0/kubernetes/install/#mounting-the-bpf-fs-optional
func (coll *Collection) Pin(dirName string, fileMode os.FileMode) error {
	err := mkdirIfNotExists(dirName, fileMode)
	if err != nil {
		return err
	}
	if len(coll.Maps) > 0 {
		mapPath := filepath.Join(dirName, "maps")
		err = mkdirIfNotExists(mapPath, fileMode)
		if err != nil {
			return err
		}
		for k, v := range coll.Maps {
			err := v.Pin(filepath.Join(mapPath, k))
			if err != nil {
				return errors.Wrapf(err, "map %s", k)
			}
		}
	}
	if len(coll.Programs) > 0 {
		progPath := filepath.Join(dirName, "programs")
		err = mkdirIfNotExists(progPath, fileMode)
		if err != nil {
			return err
		}
		for k, v := range coll.Programs {
			err = v.Pin(filepath.Join(progPath, k))
			if err != nil {
				return errors.Wrapf(err, "program %s", k)
			}
		}
	}
	return nil
}

func mkdirIfNotExists(dirName string, fileMode os.FileMode) error {
	_, err := os.Stat(dirName)
	if err != nil && os.IsNotExist(err) {
		err = os.Mkdir(dirName, fileMode)
	}
	if err != nil {
		return err
	}
	return nil
}

// LoadPinnedCollection loads a Collection from the pinned directory.
//
// Requires at least Linux 4.13, use LoadPinnedCollectionExplicit on
// earlier versions.
func LoadPinnedCollection(dirName string) (*Collection, error) {
	return loadCollection(
		dirName,
		func(_ string, path string) (*Map, error) {
			return LoadPinnedMap(path)
		},
		func(_ string, path string) (*Program, error) {
			return LoadPinnedProgram(path)
		},
	)
}

// LoadPinnedCollectionExplicit loads a Collection from the pinned directory with explicit parameters.
func LoadPinnedCollectionExplicit(dirName string, maps map[string]*MapABI, progs map[string]*ProgramABI) (*Collection, error) {
	return loadCollection(
		dirName,
		func(name string, path string) (*Map, error) {
			return LoadPinnedMapExplicit(path, maps[name])
		},
		func(name string, path string) (*Program, error) {
			return LoadPinnedProgramExplicit(path, progs[name])
		},
	)
}

func loadCollection(dirName string, loadMap func(string, string) (*Map, error), loadProgram func(string, string) (*Program, error)) (*Collection, error) {
	maps, err := readFileNames(filepath.Join(dirName, "maps"))
	if err != nil {
		return nil, err
	}
	progs, err := readFileNames(filepath.Join(dirName, "programs"))
	if err != nil {
		return nil, err
	}
	bpfColl := &Collection{
		Maps:     make(map[string]*Map),
		Programs: make(map[string]*Program),
	}
	for _, mf := range maps {
		name := filepath.Base(mf)
		m, err := loadMap(name, mf)
		if err != nil {
			return nil, errors.Wrapf(err, "map %s", name)
		}
		bpfColl.Maps[name] = m
	}
	for _, pf := range progs {
		name := filepath.Base(pf)
		prog, err := loadProgram(name, pf)
		if err != nil {
			return nil, errors.Wrapf(err, "program %s", name)
		}
		bpfColl.Programs[name] = prog
	}
	return bpfColl, nil
}

func readFileNames(dirName string) ([]string, error) {
	var fileNames []string
	files, err := ioutil.ReadDir(dirName)
	if err != nil && err != os.ErrNotExist {
		return nil, err
	}
	for _, fi := range files {
		fileNames = append(fileNames, fi.Name())
	}
	return fileNames, nil
}
