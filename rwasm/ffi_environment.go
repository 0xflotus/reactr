package rwasm

import (
	"context"
	"crypto/rand"
	"math"
	"math/big"
	"sync"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/suborbital/reactr/rt"
	"github.com/suborbital/reactr/rwasm/moduleref"
	"github.com/suborbital/vektor/vlog"
	"github.com/wasmerio/wasmer-go/wasmer"
	"golang.org/x/sync/semaphore"
)

/*
 In order to allow "easy" communication of data across the FFI barrier (outbound Go -> WASM and inbound WASM -> Go), rwasm provides
 an FFI API. Functions exported from a WASM module can be easily called by Go code via the Wasmer instance exports, but returning data
 to the host Go code is not quite as straightforward.

 In order to accomplish this, rwasm internally keeps a set of "environments" in a singleton package var (`environments` below).
 Each environment is a container that includes the WASM module bytes, and a set of WASM instances (runtimes) to execute said module.
 The envionment object has an index referencing its place in the singleton array, and each instance has an index referencing its position within
 the environment's instance array.

 When a WASM function calls one of the FFI API functions, it includes the `ident`` value that was provided at the beginning
 of job execution, which allows rwasm to look up the [env][instance] and send the result on the appropriate result channel. This is needed due to
 the way Go makes functions available on the FFI using CGO.
*/

// the globally shared set of Wasm environments, accessed by UUID
var environments = map[string]*wasmEnvironment{}

// a lock to ensure the environments array is concurrency safe (didn't use sync.Map to prevent type coersion)
var envLock = sync.RWMutex{}

// the instance mapper maps a random int32 to a wasm instance to prevent malicious access to other instances via the FFI
var instanceMapper = sync.Map{}

// the internal Logger used by the Wasm runtime system
var internalLogger = vlog.Default()

// wasmEnvironment is an environmenr in which Wasm instances run
type wasmEnvironment struct {
	UUID      string
	ref       *moduleref.WasmModuleRef
	module    *wasmer.Module
	store     *wasmer.Store
	imports   *wasmer.ImportObject
	instances []*wasmInstance

	// the index of the last used wasm instance
	instIndex int
	lock      sync.RWMutex
}

type wasmInstance struct {
	wasmerInst *wasmer.Instance

	ctx *rt.Ctx

	ffiResult []byte

	resultChan chan []byte
	errChan    chan rt.RunErr

	access *semaphore.Weighted
}

// instanceReference is a "pointer" to the global environments array and the
// wasm instances within each environment
type instanceReference struct {
	EnvUUID   string
	InstIndex int
}

// newEnvironment creates a new environment and adds it to the shared environments array
// such that Wasm instances can return data to the correct place
func newEnvironment(ref *moduleref.WasmModuleRef) *wasmEnvironment {
	envLock.Lock()
	defer envLock.Unlock()

	e := &wasmEnvironment{
		UUID:      uuid.New().String(),
		ref:       ref,
		instances: []*wasmInstance{},
		instIndex: 0,
		lock:      sync.RWMutex{},
	}

	environments[e.UUID] = e

	return e
}

func (w *wasmEnvironment) instanceAtIndex(idx int) (*wasmInstance, error) {
	w.lock.RLock()
	defer w.lock.RUnlock()

	if len(w.instances) <= idx-1 {
		return nil, errors.New("invalid instance index")
	}

	return w.instances[idx], nil
}

// addInstance adds a new Wasm instance to the environment's pool
func (w *wasmEnvironment) addInstance() error {
	w.lock.Lock()
	defer w.lock.Unlock()

	module, _, imports, err := w.internals()
	if err != nil {
		return errors.Wrap(err, "failed to ModuleBytes")
	}

	inst, err := wasmer.NewInstance(module, imports)
	if err != nil {
		return errors.Wrap(err, "failed to NewInstance")
	}

	// if the module has exported a WASI start, call it
	wasiStart, err := inst.Exports.GetWasiStartFunction()
	if err == nil && wasiStart != nil {
		if _, err := wasiStart(); err != nil {
			return errors.Wrap(err, "failed to wasiStart")
		}
	} else {
		// if the module has exported a _start function, call it
		_start, err := inst.Exports.GetFunction("_start")
		if err == nil && _start != nil {
			if _, err := _start(); err != nil {
				return errors.Wrap(err, "failed to _start")
			}
		}
	}

	// if the module has exported an init function, call it
	init, err := inst.Exports.GetFunction("init")
	if err == nil && init != nil {
		if _, err := init(); err != nil {
			return errors.Wrap(err, "failed to init")
		}
	}

	instance := &wasmInstance{
		wasmerInst: inst,
		resultChan: make(chan []byte, 1),
		errChan:    make(chan rt.RunErr, 1),
		access:     semaphore.NewWeighted(1),
	}

	w.instances = append(w.instances, instance)

	return nil
}

func (w *wasmEnvironment) removeInstance() error {
	// this process happens in a 4 step lock-dance:
	// 1. Lock, grab the last instance in the array, unlock
	// 2. Acquire the intsance's semaphore so we can be sure anyone vying for its attention is done with it
	// 3. Lock, shorten the array so that instance is no longer part of it, unlock
	// 3. Kill off the internal wasm instance and then deallocate the whole thing so it gets garbage collected

	// 1.
	w.lock.RLock()

	if len(w.instances) == 0 {
		w.lock.RUnlock()
		return nil
	}

	inst := w.instances[len(w.instances)-1]

	// unlock before attempting to acquire the instance, they can deadlock eachother
	w.lock.RUnlock()

	// 2.
	if err := inst.access.Acquire(context.Background(), 1); err != nil {
		return errors.Wrap(err, "failed to Acquire instance for removal")
	}

	// 3.
	w.lock.Lock()
	if w.instIndex == len(w.instances)-1 {
		w.instIndex--
	}

	w.instances = w.instances[:len(w.instances)-1]
	w.lock.Unlock()

	// 4.
	inst.wasmerInst.Close()
	inst.wasmerInst = nil
	inst.ctx = nil
	inst.ffiResult = nil
	inst.resultChan = nil
	inst.errChan = nil

	inst.access.Release(1)
	inst.access = nil
	inst = nil

	return nil
}

// useInstance provides an instance from the environment's pool to be used
func (w *wasmEnvironment) useInstance(ctx *rt.Ctx, instFunc func(*wasmInstance, int32)) error {
	// we have to do a lock dance between w.lock and inst.access to ensure that
	// a single instance isn't used by more than one runnable at the same time
	w.lock.Lock()

	if w.instIndex == len(w.instances)-1 {
		w.instIndex = 0
	} else {
		w.instIndex++
	}

	instIndex := w.instIndex
	inst := w.instances[instIndex]

	// now that we've got an instance, release the lock since this can deadlock with inst.access
	w.lock.Unlock()

	// acquire the instance's semaphore so we are guaranteed to be the only one using it
	inst.access.Acquire(context.Background(), 1)
	defer inst.access.Release(1)

	// generate a random identifier as a reference to the instance in use to
	// easily allow the Wasm module to reference itself when calling back over the FFI
	ident, err := setupNewIdentifier(w.UUID, instIndex)
	if err != nil {
		return errors.Wrap(err, "failed to setupNewIdentifier")
	}

	// setup the instance's temporary state
	inst.ffiResult = nil
	inst.ctx = ctx

	// do the actual call into the Wasm module
	instFunc(inst, ident)

	// clear the instance's temporary state
	inst.ctx = nil
	inst.ffiResult = nil

	// remove the instance from global state
	removeIdentifier(ident)

	return nil
}

func (w *wasmEnvironment) internals() (*wasmer.Module, *wasmer.Store, *wasmer.ImportObject, error) {
	if w.module == nil {
		moduleBytes, err := w.ref.Bytes()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to get ref ModuleBytes")
		}

		engine := wasmer.NewEngine()
		store := wasmer.NewStore(engine)

		// Compiles the module
		mod, err := wasmer.NewModule(store, moduleBytes)
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to NewModule")
		}

		env, err := wasmer.NewWasiStateBuilder(w.ref.Name).Finalize()
		if err != nil {
			return nil, nil, nil, errors.Wrap(err, "failed to NewWasiStateBuilder.Finalize")
		}

		imports, err := env.GenerateImportObject(store, mod)
		if err != nil {
			imports = wasmer.NewImportObject() // for now, defaulting to creating non-WASI imports if there's a failure.
		}

		// mount the Runnable API host functions to the module's imports
		addHostFns(imports, store,
			returnResult(),
			returnError(),
			getFFIResult(),
			fetchURL(),
			graphQLQuery(),
			cacheSet(),
			cacheGet(),
			logMsg(),
			requestGetField(),
			respSetHeader(),
			getStaticFile(),
			abortHandler(),
		)

		w.module = mod
		w.store = store
		w.imports = imports
	}

	return w.module, w.store, w.imports, nil
}

func setupNewIdentifier(envUUID string, instIndex int) (int32, error) {
	for {
		ident, err := randomIdentifier()
		if err != nil {
			return -1, errors.Wrap(err, "failed to randomIdentifier")
		}

		// ensure we don't accidentally overwrite something else
		// (however unlikely that may be)
		if _, exists := instanceMapper.Load(ident); exists {
			continue
		}

		ref := instanceReference{
			EnvUUID:   envUUID,
			InstIndex: instIndex,
		}

		instanceMapper.Store(ident, ref)

		return ident, nil
	}
}

func removeIdentifier(ident int32) {
	instanceMapper.Delete(ident)
}

func instanceForIdentifier(ident int32, needsFFIResult bool) (*wasmInstance, error) {
	rawRef, exists := instanceMapper.Load(ident)
	if !exists {
		return nil, errors.New("instance does not exist")
	}

	ref := rawRef.(instanceReference)

	envLock.RLock()
	defer envLock.RUnlock()

	env, exists := environments[ref.EnvUUID]
	if !exists {
		return nil, errors.New("environment does not exist")
	}

	inst, err := env.instanceAtIndex(ref.InstIndex)
	if err != nil {
		return nil, errors.Wrap(err, "failed to instanceAtIndex")
	}

	if needsFFIResult && inst.ffiResult != nil {
		return nil, errors.New("cannot use instance for host call with existing call in progress")
	}

	return inst, nil
}

func randomIdentifier() (int32, error) {
	// generate a random number between 0 and the largest possible int32
	num, err := rand.Int(rand.Reader, big.NewInt(math.MaxInt32))
	if err != nil {
		return -1, errors.Wrap(err, "failed to rand.Int")
	}

	return int32(num.Int64()), nil
}

// UseInternalLogger sets the logger to be used log internal wasm runtime messages
func UseInternalLogger(l *vlog.Logger) {
	internalLogger = l
}

/////////////////////////////////////////////////////////////////////////////
// below is the wasm glue code used to manipulate wasm instance memory     //
// this requires a set of functions to be available within the wasm module //
// - allocate                                                              //
// - deallocate                                                            //
/////////////////////////////////////////////////////////////////////////////

func (w *wasmInstance) setFFIResult(data []byte) error {
	if w.ffiResult != nil {
		return errors.New("instance ffiResult is already set")
	}

	w.ffiResult = data

	return nil
}

func (w *wasmInstance) useFFIResult() ([]byte, error) {
	if w.ffiResult == nil {
		return nil, errors.New("instance ffiResult is not set")
	}

	defer func() {
		w.ffiResult = nil
	}()

	return w.ffiResult, nil
}

func (w *wasmInstance) readMemory(pointer int32, size int32) []byte {
	memory, err := w.wasmerInst.Exports.GetMemory("memory")
	if err != nil || memory == nil {
		// we failed
		return []byte{}
	}

	data := memory.Data()[pointer:]
	result := make([]byte, size)

	for index := 0; int32(index) < size; index++ {
		result[index] = data[index]
	}

	return result
}

func (w *wasmInstance) writeMemory(data []byte) (int32, error) {
	lengthOfInput := len(data)

	allocate, err := w.wasmerInst.Exports.GetFunction("allocate")
	if err != nil || allocate == nil {
		return -1, errors.New("missing required FFI function: allocate")
	}

	// Allocate memory for the input, and get a pointer to it.
	allocateResult, err := allocate(lengthOfInput)
	if err != nil {
		return -1, errors.Wrap(err, "failed to call allocate")
	}

	pointer := allocateResult.(int32)

	w.writeMemoryAtLocation(pointer, data)

	return pointer, nil
}

func (w *wasmInstance) writeMemoryAtLocation(pointer int32, data []byte) {
	memory, err := w.wasmerInst.Exports.GetMemory("memory")
	if err != nil || memory == nil {
		// we failed
		return
	}

	scopedMemory := memory.Data()[pointer:]

	copy(scopedMemory, data)
}

func (w *wasmInstance) deallocate(pointer int32, length int) {
	dealloc, err := w.wasmerInst.Exports.GetFunction("deallocate")
	if err != nil || dealloc == nil {
		// we failed
		return
	}

	dealloc(pointer, length)
}
