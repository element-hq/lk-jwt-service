package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v5"
)


var DelayedEventsEndpoint = "/_matrix/client/unstable/org.matrix.msc4140/delayed_events"
//var DelayedEventsEndpoint =  "/_matrix/client/v1/delayed_events"

type DelayEventAction string
const (
	ActionRestart DelayEventAction = "restart"
	ActionSend    DelayEventAction = "send"
)

//go:generate stringer -type=DelayEventState
type DelayEventState int

const (
	WaitingForInitialConnect DelayEventState = iota
	Connected
	Disconnected
	Completed
	Replaced
)

//go:generate stringer -type=DelayedEventSignal
type DelayedEventSignal int
const (
	ParticipantConnected DelayedEventSignal = iota
	ParticipantLookupSuccessful
	ParticipantDisconnectedIntentionally
	ParticipantConnectionAborted
	DelayedEventReset
	DelayedEventTimedOut
	DelayedEventNotFound
	WaitingStateTimedOut
	SFUNotAvailable
)

type LiveKitRoomAlias string
type LiveKitIdentity string

type SFUMessage struct {
	Type            DelayedEventSignal
	LiveKitIdentity LiveKitIdentity
}

type MonitorMessage struct {
	// Types of messages:
	// - ClientConnectedToSFU
	// - ClientDisconnectedFromSFU
	// - ResetDelayedEventJob
	// - SentDelayedEventJob

	LiveKitIdentity LiveKitIdentity
	State           DelayEventState
	Event           DelayedEventSignal
	JobId           UniqueID
}

type DelayedEventTimer struct {
	sync.Mutex
	wg                *sync.WaitGroup
	timer             *time.Timer
	timeout           time.Time
}

func NewDelayedEventTimer(wg *sync.WaitGroup, restartDuration time.Duration, timeoutDuration time.Duration, f func()) *DelayedEventTimer {
	dt := &DelayedEventTimer{
		wg: wg,
	}
	dt.timeout = time.Now().Add(timeoutDuration)
	if restartDuration <= 0 {
		restartDuration = timeoutDuration
	}
	dt.wg.Add(1)
	dt.timer = time.AfterFunc(
		restartDuration, 
		func() {
			defer dt.wg.Done()
			f()
		},
	)
	return dt
}

func (dt *DelayedEventTimer) Reset(restartDuration time.Duration, timeoutDuration time.Duration) bool {
	dt.Lock()
	defer dt.Unlock()
	if restartDuration <= 0 {
		restartDuration = timeoutDuration
	}
	dt.timeout = time.Now().Add(timeoutDuration)
	if dt.timer != nil {
		dt.wg.Add(1)
		dt.timer.Reset(restartDuration)
		return true
	}
	return false
}

func (dt *DelayedEventTimer) Stop() bool {
	dt.Lock()
	defer dt.Unlock()
	if dt.timer != nil {
		isStopped := dt.timer.Stop()
		if isStopped {
			dt.wg.Done()
		}
	   return isStopped
	}
	// Non existing timer is considered not as transitioning to stopped state
	return false
}

func (dt *DelayedEventTimer) TimeRemaining() time.Duration {
	dt.Lock()
	defer dt.Unlock()
	remaining := time.Until(dt.timeout)
	if remaining < 0 {
		return 0
	}
	return remaining
}

type DelayedEventJob struct {
	sync.Mutex
	ctx                  context.Context
	cancel               context.CancelFunc
	wg                   sync.WaitGroup
	JobId                UniqueID
	CsApiUrl             string
	DelayId              string
	DelayTimeout         time.Duration
	LiveKitRoom          LiveKitRoomAlias
	LiveKitIdentity      LiveKitIdentity
	State                DelayEventState
	MonitorChannel       chan<- MonitorMessage
	EventChannel         chan DelayedEventSignal
	fsmTimerWaitingState *time.Timer
	fsmTimerDelayedEvent *DelayedEventTimer
}

func (job *DelayedEventJob) String() string {
	return fmt.Sprintf("DelayedEventJob{CSAPI: %s, DelayId: %s, DelayTimeout: %s, LiveKitRoom: %s, LiveKitIdentity: %s, State: %d}",
		job.CsApiUrl,
		job.DelayId,
		job.DelayTimeout.String(),
		job.LiveKitRoom,
		job.LiveKitIdentity,
		job.State,
	)
}

func (job *DelayedEventJob) setState(state DelayEventState) {
	job.Lock()
	defer job.Unlock()
	job.State = state
	slog.Debug("Job: FSM -> State set", "newState", state, "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId)
}

func (job *DelayedEventJob) getState() DelayEventState {
	job.Lock()
	defer job.Unlock()
	return job.State
}

func (job *DelayedEventJob) DelayRestartDuration() time.Duration {
	// Delay restart duration is 80% of original timeout
	job.Lock()
	defer job.Unlock()
	return job.DelayTimeout * 8 / 10
}

func (job *DelayedEventJob) GetFsmTimerDelayedEventRemainingTime() time.Duration {
	job.Lock()
	defer job.Unlock()
	if job.fsmTimerDelayedEvent != nil {
		return job.fsmTimerDelayedEvent.TimeRemaining()
	}
	return -1
}

func (job *DelayedEventJob) SetFsmTimerDelayedEvent(t *DelayedEventTimer) {
	job.Lock()
	defer job.Unlock()
	job.fsmTimerDelayedEvent = t
}

func (job *DelayedEventJob) GetFsmTimerDelayedEvent() *DelayedEventTimer {
	job.Lock()
	defer job.Unlock()
	return job.fsmTimerDelayedEvent
}

func (job *DelayedEventJob) StopFsmTimerDelayedEvent() bool {
	job.Lock()
	defer job.Unlock()
	if job.fsmTimerDelayedEvent != nil {
		// Note the wg.Done() handling is done inside DelayedEventTimer.Stop()
		return job.fsmTimerDelayedEvent.Stop()
	}
	// Non existing timer is considered not as transitioned to stopped state
	return false
}

func (job *DelayedEventJob) StopFsmTimerWaitingState() bool {
	job.Lock()
	defer job.Unlock()
	if job.fsmTimerWaitingState != nil {
		isStopped := job.fsmTimerWaitingState.Stop()
		if isStopped {
			job.wg.Done()
		}
		return isStopped
	}
	// Non existing timer is considered not as transitioned to stopped state
	return false
}

func (job *DelayedEventJob) handleEvent(event DelayedEventSignal) bool {
	slog.Debug("Job: FSM -> Event", "event", event, "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId)
	switch event {
	case ParticipantConnected:
		return job.handleEventParticipantConnected(event)
	case ParticipantLookupSuccessful:
		return job.handleEventParticipantLookupSuccessful(event)
	case ParticipantDisconnectedIntentionally:
		return job.handleEventParticipantDisconnected(event)
	case ParticipantConnectionAborted:
		return job.handleEventParticipantConnectionAborted(event)
	case DelayedEventReset:
		return job.handleEventDelayedEventReset(event)
	case DelayedEventTimedOut:
		return job.handleEventDelayedEventTimedOut(event)
	case DelayedEventNotFound:
		return job.handleEventDelayedEventNotFound(event)
	case WaitingStateTimedOut:
		return job.handleEventWaitingStateTimedOut(event)
	case SFUNotAvailable:
		// noop
	default:
		slog.Error("Job: FSM -> Event: Received unknown Event", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "event", event)
	}
	return false
}

func (job *DelayedEventJob) handleStateEntryAction(event DelayedEventSignal) {
	switch job.getState() {
	case WaitingForInitialConnect:
		// Handle waiting for initial connect state
	case Connected:

		// Handle connected state

		// The timerWaitingState is used as a sufficient criteria during WaitingForInitialConnect state
		// to transition to Disconnected state. However, once in Connected state we do not need it anymore
		job.StopFsmTimerWaitingState()

		// Create a Finate State Machine maintaining the MatrixRTC Session's delayed disconnect event
		// - Maintains the remaining time until the delayed event times out (and will be sent by the 
		//   Matrix Homeserver)
		// - Periodically issues the "DelayedEventReset" event to self which in turn handles (with 
		//   sufficient headroom) the actual restart action towards the Matrix Homeserver.
		// Note that NewDelayedEventTimer will maintain the wg counter for us
		job.SetFsmTimerDelayedEvent(NewDelayedEventTimer(&job.wg, job.DelayRestartDuration(), job.DelayTimeout, func() {
			slog.Debug("Job: FSM DelayedEvent -> Event: ResetTimerExpired", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId)
			job.EventChannel <- DelayedEventReset
		}))

		// As we do not now how much time has been elapsed during the creation of the delayed event 
		// and connecting to the Matrix Authorisation Service we immediately trigger the delayed event 
		// reset action to sync the internal timer.
		job.StopFsmTimerDelayedEvent() // Note the timer will be re-started as part of DelayedEventReset
		job.EventChannel <- DelayedEventReset
	case Disconnected:

		remainingSnapshot := job.GetFsmTimerDelayedEventRemainingTime()
		job.StopFsmTimerDelayedEvent()
		job.StopFsmTimerWaitingState()

		// Create an exponential backoff policy
		expBackOff := backoff.NewExponentialBackOff()
		expBackOff.InitialInterval = 1000 * time.Millisecond
		expBackOff.Multiplier = 1.5
		expBackOff.RandomizationFactor = 0.5
		expBackOff.MaxInterval = 60 * time.Second

		operation := func() (*http.Response, error) {
			return helperExecuteDelayedEventAction(job.CsApiUrl, job.DelayId, ActionSend)
		}
		
		resp, err := backoff.Retry(
			job.ctx,
			operation,
			backoff.WithBackOff(expBackOff),
			backoff.WithMaxElapsedTime(remainingSnapshot),
		)
		
		if err != nil {
			slog.Warn(
				"Job: StateEntryAction -> ExecuteDelayedEventAction (ActionSend): Error while issuing action",
				"state", Disconnected, "event", event,
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
			)
			// TODO ???
		}

		// If we exited loop without a successful response, notify and return
		if resp == nil || resp.StatusCode < 200 || (resp.StatusCode >= 300 && resp.StatusCode != 404) {
			slog.Warn(
				"Job: StateEntryAction -> ExecuteDelayedEventAction (ActionSend): Error while issuing action within remaining time",
				"state", Disconnected, "event", event,
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
			)
			// TODO ???
		}

		if resp != nil && resp.StatusCode == 404 {
			slog.Info(
				"Job: StateEntryAction -> ExecuteDelayedEventAction (ActionSend): DelayedEventNotFound (already sent or cancelled)",
				"state", Disconnected, "event", event,
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
			)
		}
		
		job.MonitorChannel <- MonitorMessage{
			LiveKitIdentity: job.LiveKitIdentity,
			State:           Disconnected,
			Event:           event,
			JobId:           job.JobId,
		}
		slog.Debug(
			"Job: StateEntryAction -> Emit event <MonitorMessage->Disconnected>",
			"state", Disconnected, "event", event,
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
		)

	case Completed:
		job.StopFsmTimerDelayedEvent()
		job.StopFsmTimerWaitingState()

		job.MonitorChannel <- MonitorMessage{
			LiveKitIdentity: job.LiveKitIdentity,
			State:           Completed,
			Event:           event,
			JobId:           job.JobId,
		}
		slog.Debug(
			"Job: StateEntryAction -> Emit event <MonitorMessage->Completed>",
			"state", Completed, "event", event,
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "jobId", job.JobId,
		)
	}
}

func (job *DelayedEventJob) handleEventParticipantConnected(event DelayedEventSignal) (stateChanged bool) {
	if job.getState() == WaitingForInitialConnect {
		job.setState(Connected)
		slog.Info(
			"Job: State -> Connected (by Event: ParticipantConnected)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantLookupSuccessful(event DelayedEventSignal) (stateChanged bool) {
	if job.getState() == WaitingForInitialConnect {
		job.setState(Connected)
		slog.Info(
			"Job: State -> Connected (by Event: ParticipantLookupSuccessful)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantDisconnected(event DelayedEventSignal) (stateChanged bool) {
	if job.getState() == Connected {
		job.setState(Disconnected)
		slog.Info(
			"Job: State -> Disconnected (by Event: ParticipantDisconnected)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventParticipantConnectionAborted(event DelayedEventSignal) (stateChanged bool) {
	if job.getState() == Connected {
		job.setState(Completed)
		slog.Info(
			"Job: State -> Completed (by Event: ParticipantConnectionAborted)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventTimedOut(event DelayedEventSignal) (stateChanged bool) {
	curState := job.getState()
	if curState == WaitingForInitialConnect || curState == Connected {
		job.setState(Disconnected)
		slog.Info(
			"Job: State -> Disconnected (by Event: DelayedEventTimedOut)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventNotFound(event DelayedEventSignal) (stateChanged bool) {
	curState := job.getState()
	if curState == WaitingForInitialConnect || curState == Connected {
		job.setState(Disconnected)
		slog.Info(
			"Job: State -> Disconnected (by Event: DelayedEventNotFound; already sent or cancelled)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) handleEventDelayedEventReset(event DelayedEventSignal) (stateChanged bool) {
	curState := job.getState()
	fsmDelayedEventTimer := job.GetFsmTimerDelayedEvent()

	if (curState == Connected || curState == WaitingForInitialConnect) && fsmDelayedEventTimer != nil {
		remainingSnapshot := job.GetFsmTimerDelayedEventRemainingTime()

		// This is addressing
		//  - A potential race condition from the StateEntryAction of Connected state
		//    where we stop the timer (job.FsmTimerDelayedEventStop()) and immediately trigger a reset 
		//    event to sync the timer.
		//  - The general case of races where the remaining time is already elapsed due to delays in
		//    processing
		if remainingSnapshot <= 0 {
			// instead of issuing DelayedEventTimedOut event we directly transition to Disconnected state here for simplicity
			job.setState(Disconnected)
			slog.Info(
				"Job: State -> Disconnected (by Event: DelayedEventTimedOut)", 
				"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
			)
			return true
		}

		operation := func() (*http.Response, error) {
			return helperExecuteDelayedEventAction(job.CsApiUrl, job.DelayId, ActionRestart)
		}
		/*
		operation := func()(*http.Response, error){log.Printf("huhu");return &http.Response{
			Status:        "200 OK",
			StatusCode:    200,
			Proto:         "HTTP/1.1",
			ProtoMajor:    1,
			ProtoMinor:    1,
			Body:          ioutil.NopCloser(bytes.NewBufferString(url)),
			ContentLength: int64(len(url)),
			Header:        make(http.Header, 0),
		}, nil}
		*/

		job.wg.Add(1)
		go func(t *DelayedEventTimer, remaining time.Duration, timeout time.Duration, nextReset time.Duration,lkRm LiveKitRoomAlias, lkId LiveKitIdentity, ch chan DelayedEventSignal) {
			defer job.wg.Done()

			// Create an exponential backoff policy with defaults
			expBackOff := backoff.NewExponentialBackOff()
			expBackOff.InitialInterval = 1000 * time.Millisecond
			expBackOff.Multiplier = 1.5
			expBackOff.RandomizationFactor = 0.5
			expBackOff.MaxInterval = 60 * time.Second
			
			resp, err := backoff.Retry(
				job.ctx,
				operation,
				backoff.WithBackOff(expBackOff),
				backoff.WithMaxElapsedTime(remaining),
			)

			if err != nil {
				slog.Warn(
					"Job: FSM DelayedEvent -> ExecuteDelayedEventAction (ActionRestart): Error while issuing action (Emit event <DelayedEventTimedOut>)", 
					"room", lkRm, "lkId", lkId, "jobId", job.JobId,
					"err", err,
				)
				ch <- DelayedEventTimedOut
				return
			}

			if resp == nil || resp.StatusCode == 404 {
				slog.Warn(
					"Job: FSM DelayedEvent -> ExecuteDelayedEventAction (ActionRestart): Already sent or cancelled (Emit event <DelayedEventSent>)", 
					"room", lkRm, "lkId", lkId, "jobId", job.JobId,
				)
				ch <- DelayedEventNotFound
				return
			}

			// If we exited loop without a successful response, notify and return
			if resp == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
				slog.Warn(
					"Job: FSM DelayedEvent -> ExecuteDelayedEventAction (ActionRestart): Error while issuing action within remaining time (Emit event <DelayedEventTimedOut>)", 
					"room", lkRm, "lkId", lkId, "jobId", job.JobId,
				)
				ch <- DelayedEventTimedOut
				return
			}

			// Schedule next reset in 80% of original timeout, but do not exceed remaining TTL
			t.Reset(nextReset, timeout)
			slog.Debug(fmt.Sprintf("Job: FSM DelayedEvent -> Event: ExecuteDelayedEventAction->ActionRestart (scheduled next reset action in %s)", nextReset), "room", lkRm, "lkId", lkId)
		}(
			fsmDelayedEventTimer,
			remainingSnapshot,
			job.DelayTimeout, job.DelayRestartDuration(),
			job.LiveKitRoom,
			job.LiveKitIdentity, 
			job.EventChannel,
		)
		return false
	} 
	return false
}

func (job *DelayedEventJob) handleEventWaitingStateTimedOut(event DelayedEventSignal) (stateChanged bool) {
	job.Lock()
	defer job.Unlock()
	if job.State == WaitingForInitialConnect {
		job.State = Disconnected
		slog.Info(
			"Job: State -> Disconnected (by Event: WaitingStateTimedOut)", 
			"room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId,
		)
		return true
	}
	return false
}

func (job *DelayedEventJob) Close() {
	job.cancel()
	job.StopFsmTimerWaitingState()
	job.StopFsmTimerDelayedEvent()
	close(job.EventChannel)
	slog.Debug("Job -> closed", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)}

func NewDelayedEventJob(parentCtx context.Context, csApiUrl string, delayId string, delayTimeout time.Duration, liveKitRoom LiveKitRoomAlias, liveKitIdentity LiveKitIdentity, MonitorChannel chan<- MonitorMessage) (*DelayedEventJob, error) {
	if delayTimeout <= 0 {
		return nil, fmt.Errorf("invalid delay timeout for delayed event job: %d", delayTimeout)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	job := &DelayedEventJob{
		ctx:             ctx,
		cancel:          cancel,
		JobId:           NewUniqueID(),
		CsApiUrl:        csApiUrl,
		DelayId:         delayId,
		DelayTimeout:    delayTimeout,
		LiveKitRoom:     liveKitRoom,
		LiveKitIdentity: liveKitIdentity,
		State:           WaitingForInitialConnect,
		MonitorChannel:  MonitorChannel,
		EventChannel:    make(chan DelayedEventSignal, 10),
	}

	job.wg.Add(1)
	go func() {
		defer job.wg.Done()

		for {
			select {
			case <-job.ctx.Done():
				slog.Debug("Job: Dispatching Events -> goroutine done", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
				return
			case event := <- job.EventChannel:
				slog.Debug("Job: Dispatching Event", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "event", event)
				stateChanged := job.handleEvent(event)
				if stateChanged {
					job.handleStateEntryAction(event)
				}
			}
		}
	}()

	var waitingDuration = min(time.Hour, job.DelayTimeout)

	job.wg.Add(1)
	job.fsmTimerWaitingState = time.AfterFunc(waitingDuration, func() {
		defer job.wg.Done()

		slog.Debug("Job: FSM WaitingState -> Event: WaitingStateTimedOut", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
		job.EventChannel <- WaitingStateTimedOut
	})

	return job, nil
}

// LiveKitRoomMonitor manages DelayedEventJobs for a specific LiveKit Room.

type LiveKitRoomMonitor struct {
	sync.Mutex
	ctx             context.Context
	cancel          context.CancelFunc
	tearingDown     bool
	upcomingJobs    int
	MonitorId       UniqueID
	lkAuth          *LiveKitAuth
	RoomAlias       LiveKitRoomAlias
	jobs            map[LiveKitIdentity]*DelayedEventJob
	JobCommChan     chan MonitorMessage
	SFUCommChan     chan SFUMessage
	HandlerCommChan chan HandlerMessage
	wg              sync.WaitGroup
}

func NewLiveKitRoomMonitor(parentCtx context.Context, lkAuth *LiveKitAuth, roomAlias LiveKitRoomAlias) *LiveKitRoomMonitor {

	ctx, cancel := context.WithCancel(parentCtx)

	monitor := &LiveKitRoomMonitor{
		ctx:          ctx,
		cancel:       cancel,
		wg:           sync.WaitGroup{},
		tearingDown:  false,
		upcomingJobs: 0,
		MonitorId:    NewUniqueID(),
		lkAuth:       lkAuth,
		RoomAlias:    roomAlias,
		jobs:         make(map[LiveKitIdentity]*DelayedEventJob),
		JobCommChan:  make(chan MonitorMessage),
		SFUCommChan:  make(chan SFUMessage, 100),
	}

	// Dispatching goroutine for handling Events from Jobs and SFU
	monitor.wg.Add(1)
	go func() {
		defer monitor.wg.Done()

		for {
			select {
			case event := <- monitor.JobCommChan:
				slog.Debug("RoomMonitor Dispatching: MonitorMessage received", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity, "event", event.Event)
				_, ok := monitor.GetJob(event.LiveKitIdentity)
				if !ok {
					slog.Warn("RoomMonitor: Job not found", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
					continue
				}
				switch event.State {
				case Disconnected:                    
					slog.Debug("RoomMonitor: Removing Job (Event: Disconnected)", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
					monitor.RemoveJob(event.LiveKitIdentity, event.JobId)
				case Completed:
					slog.Debug("RoomMonitor: Removing Job (Event: Completed)", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
					monitor.RemoveJob(event.LiveKitIdentity, event.JobId)
				 default:
					slog.Warn("RoomMonitor: MonitorMessage received with unhandled State", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity, "JobId", event.JobId, "state", event.State)
				}
			case event := <-monitor.SFUCommChan:
				switch event.Type {
				case ParticipantConnected:
					slog.Debug("FSM SFU: Event -> ParticipantConnected", "lkId", event.LiveKitIdentity)
					if job, ok := monitor.GetJob(event.LiveKitIdentity); ok {
						job.EventChannel <- ParticipantConnected
					}
				case ParticipantLookupSuccessful:
					slog.Debug("FSM SFU: Event -> ParticipantLookupSuccessful", "lkId", event.LiveKitIdentity)
					if job, ok := monitor.GetJob(event.LiveKitIdentity); ok {
						job.EventChannel <- ParticipantLookupSuccessful
					}
				case ParticipantDisconnectedIntentionally:
					slog.Debug("FSM SFU: Event -> ParticipantDisconnectedIntentionally", "lkId", event.LiveKitIdentity)
					if job, ok := monitor.GetJob(event.LiveKitIdentity); ok {
						job.EventChannel <- ParticipantDisconnectedIntentionally
					}
				case ParticipantConnectionAborted:
					slog.Debug("FSM SFU: Event -> ParticipantConnectionAborted", "lkId", event.LiveKitIdentity)
					if job, ok := monitor.GetJob(event.LiveKitIdentity); ok {
						job.EventChannel <- ParticipantConnectionAborted
					}
				}
			case <-monitor.ctx.Done():
				slog.Debug("RoomMonitor: Dispatching goroutine for handling Events from Jobs and SFU done.", "room", monitor.RoomAlias)
				return
			}
		}
	}()

	slog.Info("LiveKitRoomMonitor: Started", "room", roomAlias, "monitorId", monitor.MonitorId)

	return monitor
}

func (m *LiveKitRoomMonitor) Close() {
	m.cancel()
	m.wg.Wait()
	slog.Debug("RoomMonitor -> closed", "room", m.RoomAlias, "monitorId", m.MonitorId)
}

func (m *LiveKitRoomMonitor) GetJob(name LiveKitIdentity) (*DelayedEventJob, bool) {
	m.Lock()
	defer m.Unlock()
	job, ok := m.jobs[name]
	return job, ok
}

func (m *LiveKitRoomMonitor) AddJob(job *DelayedEventJob) bool {
	m.Lock()
	defer m.Unlock()

	if m.tearingDown {
		slog.Warn("RoomMonitor: Attempt to add job to closed monitor", "room", m.RoomAlias,"monitorId", m.MonitorId, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId)
		return false
	}

	m.jobs[job.LiveKitIdentity] = job
	slog.Info("RoomMonitor: Added Job", "room", m.RoomAlias, "monitorId", m.MonitorId, "lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId)

	var waitingDuration = min(time.Hour, job.DelayTimeout)

	opParticipantLookup := func() (bool, error) {
		return helperLiveKitParticipantLookup(job.ctx, *m.lkAuth, job.LiveKitRoom, job.LiveKitIdentity, m.SFUCommChan)
	}

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		// Create an exponential backoff policy
		expBackOff := backoff.NewExponentialBackOff()
		expBackOff.InitialInterval = 1000 * time.Millisecond
		expBackOff.Multiplier = 1.5
		expBackOff.RandomizationFactor = 0.5
		expBackOff.MaxInterval = 60 * time.Second
		
		_, err := backoff.Retry(
			job.ctx,
			opParticipantLookup,
			backoff.WithBackOff(expBackOff),
			backoff.WithMaxElapsedTime(waitingDuration),
		)

		if err != nil {
			slog.Warn("RoomMonitor: Error while looking up LiveKit participant", "room", job.LiveKitRoom, "monitorId", m.MonitorId,"lkId", job.LiveKitIdentity, "delayId", job.DelayId, "jobId", job.JobId, "err", err)
		}
	}()

	return true
}

func (m *LiveKitRoomMonitor) RemoveJob(name LiveKitIdentity, jobId UniqueID) {
	m.Lock()
	defer m.Unlock()
	if job, ok := m.jobs[name]; ok {
		if job.JobId != jobId {
			slog.Error("RoomMonitor: Ignore attempt to remove job with mismatching JobId", "room", m.RoomAlias, "lkId", name, "jobId", job.JobId, "requestedJobId", jobId)
			return
		}
		// move teardown outside of the lock
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			job.Close()
		}()
		delete(m.jobs, name)
	}

	slog.Info("RoomMonitor: Removed delayed event job", "room", m.RoomAlias, "lkId", name, "monitorId", m.MonitorId, "jobId", jobId, "leftNumJobs", len(m.jobs))

	if len(m.jobs) == 0 && m.upcomingJobs == 0 {
		m.tearingDown = true
		m.HandlerCommChan <- HandlerMessage{
			RoomAlias: m.RoomAlias,
			Event:     NoJobsLeft,
			MonitorId: m.MonitorId,
		}
		slog.Info("RoomMonitor: Emit event <HandlerMessage->NoJobsLeft>", "room", m.RoomAlias, "lkId", name, "monitorId", m.MonitorId, "jobId", jobId)
	}
}

func (m *LiveKitRoomMonitor) StartJobHandover() (release func() bool, ok bool) {
	m.Lock()
	defer m.Unlock()
	if m.tearingDown {
		return nil, false
	}
	
	m.upcomingJobs++

	m.wg.Add(1)
	release = func() bool {
		defer m.wg.Done()

		m.Lock()
		defer m.Unlock()
		
		if m.tearingDown {
			return false
		}
		if m.upcomingJobs <= 0 {
			slog.Warn("RoomMonitor: Attempt to release upcoming job when none are registered", "room", m.RoomAlias, "monitorId", m.MonitorId)
			return false
		}   
		m.upcomingJobs--
		return true
	}

	return release, true
}

func (m *LiveKitRoomMonitor) HandoverJob(jobDescription *DelayedEventJob) (bool, UniqueID) {
	job, err := NewDelayedEventJob(
		m.ctx,
		jobDescription.CsApiUrl,
		jobDescription.DelayId,
		jobDescription.DelayTimeout,
		jobDescription.LiveKitRoom,
		jobDescription.LiveKitIdentity,
		m.JobCommChan,
	)
	slog.Debug("Handler: Created delayed event job", "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "DelayId", jobDescription.DelayId, "JobId", job.JobId)
	if err != nil {
		slog.Error("Error creating delayed event job",  "room", jobDescription.LiveKitRoom, "lkId", jobDescription.LiveKitIdentity, "JobId", job.JobId, "err", err)
		return false, UniqueID("")
	}    
	slog.Debug("RoomMonitor: Adding delayed event job", "room", m.RoomAlias, "lkId", job.LiveKitIdentity)
	existingJob, ok := m.GetJob(job.LiveKitIdentity)
	if ok {
		// TODO: Discuss again if replacing here is the best strategy
		// - Pros: Ensures that only the latest job is active, preventing multiple jobs for the same participant
		// - Cons: Previous job is forcefully closed
		// Alternatives include:
		// - Accepting the new job if the old job does not anymore exist on the homeserver (404 on lookup)???
		
		// This should never happen as UniqueID is chronologically sorted (timestamp-based)
		if job.JobId <= existingJob.JobId {
			slog.Error("RoomMonitor: New JobId is not greater than existing JobId", "room", m.RoomAlias, "existingJobId", existingJob.JobId, "newJobId", job.JobId)
			panic(0)
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			slog.Warn(
				"RoomMonitor: Replacing already existing Job", 
				"room", m.RoomAlias, "lkId", existingJob.LiveKitIdentity, 
				"delayId", existingJob.DelayId, "newDelayId", job.DelayId, 
				"existingJobId", existingJob.JobId, "newJobId", job.JobId,
			)
			existingJob.setState(Replaced)
			existingJob.Close()
		}()
	}
	return m.AddJob(job), job.JobId
}