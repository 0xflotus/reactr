package rt

import (
	"encoding/json"

	"github.com/pkg/errors"
	"github.com/suborbital/grav/grav"
	"github.com/suborbital/vektor/vlog"
)

// MsgTypeReactrJobErr and others are Grav message types used for Reactr job
const (
	MsgTypeReactrJobErr    = "reactr.joberr" // any kind of error from a job run
	MsgTypeReactrRunErr    = "reactr.runerr" // specifically a RunErr returned from a Wasm Runnable
	MsgTypeReactrResult    = "reactr.result"
	MsgTypeReactrNilResult = "reactr.nil"
)

// JobFunc is a function that runs a job of a predetermined type
type JobFunc func(interface{}) *Result

// Reactr represents the main control object
type Reactr struct {
	core        *core
	defaultCaps *Capabilities
	log         *vlog.Logger
}

// New returns a Reactr ready to accept Jobs
func New() *Reactr {
	logger := vlog.Default()

	core := newCore(logger)

	r := &Reactr{
		core:        core,
		defaultCaps: DefaultCaps(),
		log:         logger,
	}

	return r
}

// Do schedules a job to be worked on and returns a result object
func (r *Reactr) Do(job Job) *Result {
	return r.DoWithCaps(job, nil)
}

// DoWithCaps schedules a job with a custom Capabilities set
// passing nil caps will cause the worker's default caps to be used
func (r *Reactr) DoWithCaps(job Job, caps *Capabilities) *Result {
	if caps != nil {
		caps.doFunc = r.core.do
		job.caps = caps
	}

	return r.core.do(&job)
}

// Schedule adds a new Schedule to the instance, Reactr will 'watch' the Schedule
// and Do any jobs when the Schedule indicates it's needed
func (r *Reactr) Schedule(s Schedule) {
	r.core.watch(s)
}

// Register registers a Runnable with the Reactr and returns a shortcut function to run those jobs
func (r *Reactr) Register(jobType string, runner Runnable, options ...Option) JobFunc {
	r.RegisterWithCaps(jobType, runner, r.defaultCaps, options...)

	helper := func(data interface{}) *Result {
		job := NewJob(jobType, data)

		return r.Do(job)
	}

	return helper
}

// RegisterWithCaps registers a Runnable with the provided Capabilities
func (r *Reactr) RegisterWithCaps(jobType string, runner Runnable, caps *Capabilities, options ...Option) {
	caps.doFunc = r.core.do

	r.core.register(jobType, runner, caps, options...)
}

// Listen causes Reactr to listen for messages of the given type and trigger the job of the same type.
// The message's data is passed to the runnable as the job data.
// The job's result is then emitted as a message. If an error occurs, it is logged and an error is sent.
// If the result is nil, nothing is sent.
func (r *Reactr) Listen(pod *grav.Pod, msgType string) {
	helper := func(data interface{}) *Result {
		job := NewJob(msgType, data)

		return r.Do(job)
	}

	pod.OnType(msgType, func(msg grav.Message) error {
		var replyMsg grav.Message

		result, err := helper(msg.Data()).Then()
		if err != nil {
			r.log.Error(errors.Wrapf(err, "job from message %s returned error result", msg.UUID()))

			runErr := &RunErr{}
			if errors.As(err, runErr) {
				// if a Wasm Runnable returned a RunErr, let's be sure to handle that
				replyMsg = grav.NewMsgWithParentID(MsgTypeReactrRunErr, msg.ParentID(), []byte(runErr.Error()))
			} else {
				replyMsg = grav.NewMsgWithParentID(MsgTypeReactrJobErr, msg.ParentID(), []byte(err.Error()))
			}
		} else {
			if result == nil {
				// if the job returned no result
				replyMsg = grav.NewMsgWithParentID(MsgTypeReactrNilResult, msg.ParentID(), []byte{})
			} else if resultMsg, isMsg := result.(grav.Message); isMsg {
				// if the job returned a Grav message
				resultMsg.SetReplyTo(msg.UUID())
				replyMsg = resultMsg
			} else if bytes, isBytes := result.([]byte); isBytes {
				// if the job returned bytes
				replyMsg = grav.NewMsgWithParentID(MsgTypeReactrResult, msg.ParentID(), bytes)
			} else if resultString, isString := result.(string); isString {
				// if the job returned a string
				replyMsg = grav.NewMsgWithParentID(MsgTypeReactrResult, msg.ParentID(), []byte(resultString))
			} else {
				// if the job returned something else like a struct
				resultJSON, err := json.Marshal(result)
				if err != nil {
					r.log.Error(errors.Wrapf(err, "job from message %s returned result that could not be JSON marshalled", msg.UUID()))
					replyMsg = grav.NewMsgWithParentID(MsgTypeReactrJobErr, msg.ParentID(), []byte(errors.Wrap(err, "failed to Marshal job result").Error()))
				} else {
					replyMsg = grav.NewMsgWithParentID(MsgTypeReactrResult, msg.ParentID(), resultJSON)
				}
			}
		}

		pod.ReplyTo(msg, replyMsg)

		return nil
	})
}

// IsRegistered returns true if the instance
// has a worker registered for the given jobType
func (r *Reactr) IsRegistered(jobType string) bool {
	return r.core.hasWorker(jobType)
}

// Job is a shorter alias for NewJob
func (r *Reactr) Job(jobType string, data interface{}) Job {
	return NewJob(jobType, data)
}
