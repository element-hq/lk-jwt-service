package main

import (
	"context"
	"fmt"
	"log"
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
	Event 			DelayedEventSignal
}

type LiveKitRoomMonitor struct {
    sync.Mutex
    lkAuth      *LiveKitAuth
    RoomAlias   LiveKitRoomAlias
    jobs        map[LiveKitIdentity]*DelayedEventJob
    JobCommChan chan MonitorMessage
    SFUCommChan chan SFUMessage
    HandlerCommChan           chan HandlerMessage
    wg          sync.WaitGroup
    quit        chan bool
}

type DelayedEventTimer struct {
    sync.Mutex
    timer             *time.Timer
    timeout           time.Time
}

func NewDelayedEventTimer(restartDuration time.Duration, timeoutDuration time.Duration, f func()) *DelayedEventTimer {
    dt := &DelayedEventTimer{}
    dt.timeout = time.Now().Add(timeoutDuration)
    if restartDuration <= 0 {
        restartDuration = timeoutDuration
    }
    dt.timer = time.AfterFunc(restartDuration , f)
    return dt
}

func (s *DelayedEventTimer) Reset(restartDuration time.Duration, timeoutDuration time.Duration) bool {
    s.Lock()
    defer s.Unlock()
    if restartDuration <= 0 {
        restartDuration = timeoutDuration
    }
    s.timeout = time.Now().Add(timeoutDuration)
    if s.timer != nil {
        s.timer.Reset(restartDuration)
        return true
    }
    return false
}

func (s *DelayedEventTimer) Stop() {
    s.Lock()
    defer s.Unlock()
    if s.timer != nil {
        s.timer.Stop()
    }
}

func (s *DelayedEventTimer) TimeRemaining() time.Duration {
    s.Lock()
    defer s.Unlock()
    remaining := time.Until(s.timeout)
    if remaining < 0 {
        return 0
    }
	return remaining
}

type DelayedEventJob struct {
    sync.Mutex
    CsApiUrl             string
    DelayID              string
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
    return fmt.Sprintf("DelayedEventJob{CSAPI: %s, DelayID: %s, DelayTimeout: %s, LiveKitRoom: %s, LiveKitIdentity: %s, State: %d}",
        job.CsApiUrl,
        job.DelayID,
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

func (job *DelayedEventJob) FsmTimerDelayedEventGetTimer() *DelayedEventTimer {
    job.Lock()
    defer job.Unlock()
    return job.fsmTimerDelayedEvent
}

func (job *DelayedEventJob) FsmTimerDelayedEventGetRemainingTime() time.Duration {
    job.Lock()
    defer job.Unlock()
    if job.fsmTimerDelayedEvent != nil {
        return job.fsmTimerDelayedEvent.TimeRemaining()
    }
    return -1
}

func (job *DelayedEventJob) FsmTimerDelayedEventCreate(t *DelayedEventTimer) {
    job.Lock()
    defer job.Unlock()
    job.fsmTimerDelayedEvent = t
}

func (job *DelayedEventJob) FsmTimerDelayedEventStop()  {
    job.Lock()
    defer job.Unlock()
    if job.fsmTimerDelayedEvent != nil {
        job.fsmTimerDelayedEvent.Stop()
    }
}

func (job *DelayedEventJob) FsmTimerWaitingStateStop()  {
    job.Lock()
    defer job.Unlock()
    if job.fsmTimerWaitingState != nil {
        job.fsmTimerWaitingState.Stop()
    }
}

func (job *DelayedEventJob) handleEvent(event DelayedEventSignal) bool {
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
        slog.Warn("Job received unknown Event", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "event", event)
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
        job.FsmTimerWaitingStateStop()

        // Create a simple Finate State Machine maintaining the MatrixRTC Sessions disconnect delayed event
        job.FsmTimerDelayedEventCreate(NewDelayedEventTimer(job.DelayRestartDuration(), job.DelayTimeout, func() {
            slog.Debug("FSM DelayedEvent -> Event: ResetTimerExpired", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
            job.EventChannel <- DelayedEventReset
        }))

        // As we do not now how much time has been elapsed during the creation of the delayed event 
        // and connecting to the Matrix Authorisation Service we immediately trigger Delayed Event 
        // Reset action to sync the internal timer.
        job.FsmTimerDelayedEventStop() // Note the timer will be re-started as part of DelayedEventReset
        job.EventChannel <- DelayedEventReset
    case Disconnected:

        remainingSnapshot := job.FsmTimerDelayedEventGetRemainingTime()
        job.FsmTimerDelayedEventStop()
        job.FsmTimerWaitingStateStop()

        // Create an exponential backoff policy
        expBackOff := backoff.NewExponentialBackOff()
        expBackOff.InitialInterval = 1000 * time.Millisecond
        expBackOff.Multiplier = 1.5
        expBackOff.RandomizationFactor = 0.5
        expBackOff.MaxInterval = 60 * time.Second

        operation := func() (*http.Response, error) {
            return helperExecuteDelayedEventAction(job.CsApiUrl, job.DelayID, ActionSend)
        }
        
        resp, err := backoff.Retry(
            context.Background(),
            operation,
            backoff.WithBackOff(expBackOff),
            backoff.WithMaxElapsedTime(remainingSnapshot),
        )
        
        if err != nil {
            slog.Warn("Error while issuing delayed disconnect event <send> action", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "err", err)
            // TODO ???
        }

        // If we exited loop without a successful response, notify and return
        if resp == nil || resp.StatusCode < 200 || (resp.StatusCode >= 300 && resp.StatusCode != 404) {
            slog.Warn("Error while issuing delayed disconnect event <send> action within remaining time", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
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
        }
        slog.Info("FSM Job -> State Entry Action: Disconnect -> Action: completed", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "err", err)

    case Completed:
        job.FsmTimerDelayedEventStop()
        job.FsmTimerWaitingStateStop()

        job.MonitorChannel <- MonitorMessage{
            LiveKitIdentity: job.LiveKitIdentity,
            State:           Completed,
            Event:           event,
        }
        slog.Info("FSM Job -> State Entry Action: Disconnect -> Action: completed", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
    }
}

func (job *DelayedEventJob) handleEventParticipantConnected(event DelayedEventSignal) (stateChanged bool) {
    if job.getState() == WaitingForInitialConnect {
        job.setState(Connected)
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
        slog.Info("FSM Job -> State changed: Disconnected (Event: LiveKit participant disconnected)", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
        return true
    }
    return false
}

func (job *DelayedEventJob) handleEventParticipantConnectionAborted(event DelayedEventSignal) (stateChanged bool) {
    if job.getState() == Connected {
        job.setState(Completed)
        slog.Info("FSM Job -> State changed: Completed (Event: LiveKit participant connection aborted)", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
        return true
    }
    return false
}

func (job *DelayedEventJob) handleEventDelayedEventTimedOut(event DelayedEventSignal) (stateChanged bool) {
    curState := job.getState()
    if curState == WaitingForInitialConnect || curState == Connected {
        job.setState(Disconnected)
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
    fsmDelayedEventTimer := job.FsmTimerDelayedEventGetTimer()
    

    if (curState == Connected || curState == WaitingForInitialConnect) && fsmDelayedEventTimer != nil {
        remainingSnapshot := job.FsmTimerDelayedEventGetRemainingTime()
        if remainingSnapshot <= 0 {
            job.setState(Disconnected)
            slog.Info("FSM Job -> State changed: Disconnected (Event: DelayedEvent timed out)", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
            return true
        }

        operation := func() (*http.Response, error) {
            return helperExecuteDelayedEventAction(job.CsApiUrl, job.DelayID, ActionRestart)
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


        go func(t *DelayedEventTimer, remaining time.Duration, timeout time.Duration, nextReset time.Duration,lkRm LiveKitRoomAlias, lkId LiveKitIdentity, ch chan DelayedEventSignal) {
            // Create an exponential backoff policy with defaults
            expBackOff := backoff.NewExponentialBackOff()
            expBackOff.InitialInterval = 1000 * time.Millisecond
            expBackOff.Multiplier = 1.5
            expBackOff.RandomizationFactor = 0.5
            expBackOff.MaxInterval = 60 * time.Second
            
            resp, err := backoff.Retry(
                context.Background(),
                operation,
                backoff.WithBackOff(expBackOff),
                backoff.WithMaxElapsedTime(remaining),
            )

            if err != nil {
                slog.Warn("Error while issuing delayed disconnect event <restart> action", "room", lkRm, "lkId", lkId, "err", err)
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
                slog.Warn("Error while issuing delayed disconnect event <restart> action within remaining time", "room", lkRm, "lkId", lkId)
                ch <- DelayedEventTimedOut
                return
            }

            // Schedule next reset in 80% of original timeout, but do not exceed remaining TTL
            t.Reset(nextReset, timeout)
            slog.Debug(fmt.Sprintf("FSM DelayedEvent -> Event: ActionRestart (scheduled next reset action in %s)", nextReset), "room", lkRm, "lkId", lkId)
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
        slog.Info("FSM Job -> state update: Disconnected (Event: WaitingState timed out)", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
        return true
    }
    return false
}

func (job *DelayedEventJob) Close() {
    job.FsmTimerWaitingStateStop()
    job.FsmTimerDelayedEventStop()
    close(job.EventChannel)
    slog.Debug("Job -> closed", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)}

    if delayTimeout <= 0 {
        return nil, fmt.Errorf("invalid delay timeout for delayed event job: %d", delayTimeout)
    }
    job := &DelayedEventJob{
        CsApiUrl:        csApiUrl,
        DelayID:         delayID,
        DelayTimeout:    delayTimeout,
        LiveKitRoom:     liveKitRoom,
        LiveKitIdentity: liveKitIdentity,
        State:           WaitingForInitialConnect,
        MonitorChannel:  MonitorChannel,
        EventChannel:    make(chan DelayedEventSignal, 10),
    }

    go func() {
        for event := range job.EventChannel {
            slog.Debug("Job: Dispatching Event", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "event", event)
            stateChanged := job.handleEvent(event)
            if  stateChanged {
                job.handleStateEntryAction(event)
            }
        }
    }()

    var waitingDuration = min(time.Hour, job.DelayTimeout)

    job.fsmTimerWaitingState = time.AfterFunc(waitingDuration, func() {
        slog.Debug("FSM WaitingState -> Event: WaitingStateTimedOut", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity)
        job.EventChannel <- WaitingStateTimedOut
    })

    return job, nil
}

func (m *LiveKitRoomMonitor) GetJob(name LiveKitIdentity) (*DelayedEventJob, bool) {
    m.Lock()
    defer m.Unlock()
    job, ok := m.jobs[name]
    return job, ok
}

func (m *LiveKitRoomMonitor) AddJob(name LiveKitIdentity, job *DelayedEventJob) {
    m.Lock()
    defer m.Unlock()
    m.jobs[name] = job
    m.wg.Add(1)

    var waitingDuration = min(time.Hour, job.DelayTimeout)

    opParticipantLookup := func() (bool, error) {
        return helperLiveKitParticipantLookup(context.TODO(), *m.lkAuth, job.LiveKitRoom, job.LiveKitIdentity, m.SFUCommChan)
    }

    go func() {
        expBackOff := backoff.NewExponentialBackOff()
        expBackOff.InitialInterval = 1000 * time.Millisecond
        expBackOff.Multiplier = 1.5
        expBackOff.RandomizationFactor = 0.5
        expBackOff.MaxInterval = 60 * time.Second
        
        _, err := backoff.Retry(
            context.Background(),
            opParticipantLookup,
            backoff.WithBackOff(expBackOff),
            backoff.WithMaxElapsedTime(waitingDuration),
        )

        if err != nil {
            slog.Warn("Error while looking up LiveKit participant", "room", job.LiveKitRoom, "lkId", job.LiveKitIdentity, "err", err)
        }
    }()

}

func (m *LiveKitRoomMonitor) RemoveJob(name LiveKitIdentity) {
    m.Lock()
    defer m.Unlock()
    if job, ok := m.jobs[name]; ok {
        job.Close()
        delete(m.jobs, name)
        m.wg.Done()
    }

    if len(m.jobs) == 0 {
        m.HandlerCommChan <- HandlerMessage{
            RoomAlias: m.RoomAlias,
            Event: NoJobsLeft,
        }
    }
}

func NewLiveKitRoomMonitor(lkAuth *LiveKitAuth, roomAlias LiveKitRoomAlias) *LiveKitRoomMonitor {

    monitor := &LiveKitRoomMonitor{
        lkAuth:      lkAuth,
        RoomAlias:   roomAlias,
        jobs:        make(map[LiveKitIdentity]*DelayedEventJob),
        JobCommChan: make(chan MonitorMessage),
        SFUCommChan: make(chan SFUMessage, 100),
        quit:        make(chan bool),
        wg:          sync.WaitGroup{},
    }

    // Dispatching goroutine for handling Events from Jobs and SFU
    go func() {
        for {
            select {
            case event := <- monitor.JobCommChan:
                slog.Debug("RoomMonitor: MonitorMessage received", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity, "event", event.Event)
                job, ok := monitor.GetJob(event.LiveKitIdentity)
                if !ok {
                    slog.Warn("RoomMonitor: Job not found", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
                    continue
                }
                switch event.State {
                 case Disconnected:                    
                    slog.Info("RoomMonitor: Removing Job (Event: Disconnected)", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
                    monitor.RemoveJob(event.LiveKitIdentity)
                 case Completed:
                    slog.Info("RoomMonitor: Removing Job (Event: Completed)", "room", monitor.RoomAlias, "lkId", event.LiveKitIdentity)
                    monitor.RemoveJob(event.LiveKitIdentity)
                 default:
                    log.Printf("LkId: %s, Job %s in Monitor %s changed state to %d", event.LiveKitIdentity, job.String(), monitor.RoomAlias, event.State)
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
            case <-monitor.quit:
                log.Printf("Exiting AddJobChan goroutine for Monitor %s", monitor.RoomAlias)
                //close(monitor.AddJobChan)
                //close(monitor.JobCommChan)
                return
            }
        }
    }()

    slog.Info("LiveKitRoomMonitor: Started", "room", roomAlias)

    return monitor
}

func (m *LiveKitRoomMonitor) addDelayedEventJob(job *DelayedEventJob) {
    slog.Info("RoomMonitor: Adding delayed event job", "room", m.RoomAlias, "lkId", job.LiveKitIdentity)
    existingJob, ok := m.GetJob(job.LiveKitIdentity)
    if ok {
        slog.Warn("RoomMonitor: Job already exists", "room", m.RoomAlias, "lkId", job.LiveKitIdentity)
        existingJob.setState(Replaced)
        existingJob.Close()
    }
    m.AddJob(job.LiveKitIdentity, job)
    if ok {
        m.wg.Done()
    }
}
