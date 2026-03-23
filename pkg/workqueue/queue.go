package workqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrQueueFull is returned by Submit when the queue channel is at capacity.
var ErrQueueFull = errors.New("work queue is full")

const defaultPendingPreviewLimit = 16

type Job struct {
	Name string
	Run  func(context.Context)
}

type Snapshot struct {
	Enabled        bool     `json:"enabled"`
	Workers        int      `json:"workers"`
	QueueSize      int      `json:"queue_size"`
	Queued         int      `json:"queued"`
	Active         int      `json:"active"`
	SubmittedTotal uint64   `json:"submitted_total"`
	StartedTotal   uint64   `json:"started_total"`
	FinishedTotal  uint64   `json:"finished_total"`
	FailedTotal    uint64   `json:"failed_total"`
	RejectedTotal  uint64   `json:"rejected_total"`
	RunningJobs    []string `json:"running_jobs"`
	PendingPreview []string `json:"pending_preview"`
}

type queuedJob struct {
	name string
	run  func(context.Context)
}

type Queue struct {
	workers            int
	queueSize          int
	pendingPreviewSize int

	jobs chan queuedJob

	closed atomic.Bool
	once   sync.Once

	submittedTotal atomic.Uint64
	startedTotal   atomic.Uint64
	finishedTotal  atomic.Uint64
	failedTotal    atomic.Uint64
	rejectedTotal  atomic.Uint64
	active         atomic.Int64

	mu             sync.Mutex
	runningJobs    map[int]string
	pendingPreview []string
}

func New(size, workers int) *Queue {
	if size <= 0 {
		size = 1
	}
	if workers <= 0 {
		workers = 1
	}
	return &Queue{
		workers:            workers,
		queueSize:          size,
		pendingPreviewSize: defaultPendingPreviewLimit,
		jobs:               make(chan queuedJob, size),
		runningJobs:        make(map[int]string),
		pendingPreview:     make([]string, 0, min(size, defaultPendingPreviewLimit)),
	}
}

func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		go q.worker(ctx, i)
	}
}

func (q *Queue) Submit(ctx context.Context, job Job) error {
	if job.Run == nil {
		return fmt.Errorf("work queue job run func is nil")
	}
	if q.closed.Load() {
		q.rejectedTotal.Add(1)
		return fmt.Errorf("work queue is closed")
	}

	name := job.Name
	if name == "" {
		name = "unnamed"
	}
	queued := queuedJob{name: name, run: job.Run}

	select {
	case q.jobs <- queued:
		q.submittedTotal.Add(1)
		q.mu.Lock()
		q.appendPendingPreviewLocked(name)
		q.mu.Unlock()
		return nil
	default:
		q.rejectedTotal.Add(1)
		return ErrQueueFull
	}
}

func (q *Queue) Stop() {
	q.once.Do(func() {
		q.closed.Store(true)
		close(q.jobs)
	})
}

func (q *Queue) Snapshot() Snapshot {
	q.mu.Lock()
	runningJobs := make([]string, 0, len(q.runningJobs))
	for _, name := range q.runningJobs {
		runningJobs = append(runningJobs, name)
	}
	pendingPreview := append([]string(nil), q.pendingPreview...)
	q.mu.Unlock()

	queued := len(q.jobs)
	if queued < 0 {
		queued = 0
	}

	return Snapshot{
		Enabled:        true,
		Workers:        q.workers,
		QueueSize:      q.queueSize,
		Queued:         queued,
		Active:         int(q.active.Load()),
		SubmittedTotal: q.submittedTotal.Load(),
		StartedTotal:   q.startedTotal.Load(),
		FinishedTotal:  q.finishedTotal.Load(),
		FailedTotal:    q.failedTotal.Load(),
		RejectedTotal:  q.rejectedTotal.Load(),
		RunningJobs:    runningJobs,
		PendingPreview: pendingPreview,
	}
}

func (q *Queue) worker(ctx context.Context, workerID int) {
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-q.jobs:
			if !ok {
				return
			}
			q.startJob(workerID, job.name)
			func() {
				defer q.finishJob(workerID)
				defer func() {
					if recover() != nil {
						q.failedTotal.Add(1)
					}
				}()
				job.run(ctx)
			}()
		}
	}
}

func (q *Queue) startJob(workerID int, name string) {
	q.startedTotal.Add(1)
	q.active.Add(1)
	q.mu.Lock()
	q.removePendingPreviewLocked(name)
	q.runningJobs[workerID] = name
	q.mu.Unlock()
}

func (q *Queue) finishJob(workerID int) {
	q.finishedTotal.Add(1)
	q.active.Add(-1)
	q.mu.Lock()
	delete(q.runningJobs, workerID)
	q.mu.Unlock()
}

func (q *Queue) appendPendingPreviewLocked(name string) {
	if q.pendingPreviewSize <= 0 {
		return
	}
	if len(q.pendingPreview) >= q.pendingPreviewSize {
		return
	}
	q.pendingPreview = append(q.pendingPreview, name)
}

func (q *Queue) removePendingPreviewLocked(name string) {
	for i, item := range q.pendingPreview {
		if item != name {
			continue
		}
		q.pendingPreview = append(q.pendingPreview[:i], q.pendingPreview[i+1:]...)
		return
	}
}
