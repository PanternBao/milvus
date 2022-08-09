package indexnode

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/milvus-io/milvus/internal/proto/commonpb"
	"github.com/stretchr/testify/assert"
)

type fakeTaskState int

const (
	fakeTaskInited = iota
	fakeTaskEnqueued
	fakeTaskPrepared
	fakeTaskLoadedData
	fakeTaskBuiltIndex
	fakeTaskSavedIndexes
)

type stagectx struct {
	mu           sync.Mutex
	curstate     fakeTaskState
	state2cancel fakeTaskState
	ch           chan struct{}
	closeMu      sync.Mutex
	closed       bool
	mimeTimeout  bool
}

var _ context.Context = &stagectx{}

func (s *stagectx) Deadline() (time.Time, bool) {
	return time.Now(), false
}

func (s *stagectx) closeChannel() <-chan struct{} {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return s.ch
	}
	close(s.ch)
	s.closed = true
	return s.ch
}

func (s *stagectx) Done() <-chan struct{} {
	if s.mimeTimeout {
		<-time.After(time.Second * 3)
		return s.closeChannel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.curstate == s.state2cancel {
		return s.closeChannel()
	}
	return s.ch
}

func (s *stagectx) Err() error {
	select {
	case <-s.ch:
		return fmt.Errorf("cancelled")
	default:
		return nil
	}
}

func (s *stagectx) Value(k interface{}) interface{} {
	return nil
}

func (s *stagectx) setState(state fakeTaskState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.curstate = state
}

var _taskwg sync.WaitGroup

type fakeTask struct {
	id            int
	ctx           context.Context
	state         fakeTaskState
	reterr        map[fakeTaskState]error
	retstate      commonpb.IndexState
	expectedState commonpb.IndexState
}

var _ task = &fakeTask{}

func (t *fakeTask) Name() string {
	return fmt.Sprintf("fake-task-%d", t.id)
}

func (t *fakeTask) Ctx() context.Context {
	return t.ctx
}

func (t *fakeTask) OnEnqueue(ctx context.Context) error {
	_taskwg.Add(1)
	t.state = fakeTaskEnqueued
	t.ctx.(*stagectx).setState(t.state)
	return t.reterr[t.state]
}

func (t *fakeTask) Prepare(ctx context.Context) error {
	t.state = fakeTaskPrepared
	t.ctx.(*stagectx).setState(t.state)
	return t.reterr[t.state]
}

func (t *fakeTask) LoadData(ctx context.Context) error {
	t.state = fakeTaskLoadedData
	t.ctx.(*stagectx).setState(t.state)
	return t.reterr[t.state]
}

func (t *fakeTask) BuildIndex(ctx context.Context) error {
	t.state = fakeTaskBuiltIndex
	t.ctx.(*stagectx).setState(t.state)
	return t.reterr[t.state]
}

func (t *fakeTask) SaveIndexFiles(ctx context.Context) error {
	t.state = fakeTaskSavedIndexes
	t.ctx.(*stagectx).setState(t.state)
	return t.reterr[t.state]
}

func (t *fakeTask) Reset() {
	_taskwg.Done()
}

func (t *fakeTask) SetState(state commonpb.IndexState) {
	t.retstate = state
}

func (t *fakeTask) GetState() commonpb.IndexState {
	return t.retstate
}

var (
	idLock sync.Mutex
	id     = 0
)

func newTask(cancelStage fakeTaskState, reterror map[fakeTaskState]error, expectedState commonpb.IndexState) task {
	idLock.Lock()
	newId := id
	id++
	idLock.Unlock()

	return &fakeTask{
		reterr: reterror,
		id:     newId,
		ctx: &stagectx{
			curstate:     fakeTaskInited,
			state2cancel: cancelStage,
			ch:           make(chan struct{}),
		},
		state:         fakeTaskInited,
		retstate:      commonpb.IndexState_IndexStateNone,
		expectedState: expectedState,
	}
}

func TestIndexTaskScheduler(t *testing.T) {
	Params.Init()

	scheduler := NewTaskScheduler(context.TODO(), 1024)

	scheduler.Start()

	tasks := make([]task, 0)

	tasks = append(tasks,
		newTask(fakeTaskLoadedData, nil, commonpb.IndexState_Abandoned),
		newTask(fakeTaskPrepared, nil, commonpb.IndexState_Abandoned),
		newTask(fakeTaskBuiltIndex, nil, commonpb.IndexState_Abandoned),
		newTask(fakeTaskSavedIndexes, nil, commonpb.IndexState_Finished),
		newTask(fakeTaskSavedIndexes, map[fakeTaskState]error{fakeTaskLoadedData: ErrNoSuchKey}, commonpb.IndexState_Failed),
		newTask(fakeTaskSavedIndexes, map[fakeTaskState]error{fakeTaskSavedIndexes: fmt.Errorf("auth failed")}, commonpb.IndexState_Unissued))

	for _, task := range tasks {
		assert.Nil(t, scheduler.Enqueue(task))
	}
	_taskwg.Wait()
	scheduler.Close()
	scheduler.wg.Wait()

	for _, task := range tasks[:len(tasks)-2] {
		assert.Equal(t, task.GetState(), task.(*fakeTask).expectedState)
		assert.Equal(t, task.Ctx().(*stagectx).curstate, task.Ctx().(*stagectx).state2cancel)
	}
	assert.Equal(t, tasks[len(tasks)-2].GetState(), tasks[len(tasks)-2].(*fakeTask).expectedState)
	assert.Equal(t, tasks[len(tasks)-2].Ctx().(*stagectx).curstate, fakeTaskState(fakeTaskLoadedData))
	assert.Equal(t, tasks[len(tasks)-1].GetState(), tasks[len(tasks)-1].(*fakeTask).expectedState)
	assert.Equal(t, tasks[len(tasks)-1].Ctx().(*stagectx).curstate, fakeTaskState(fakeTaskSavedIndexes))

	scheduler = NewTaskScheduler(context.TODO(), 1024)
	tasks = make([]task, 0, 1024)
	for i := 0; i < 1024; i++ {
		tasks = append(tasks, newTask(fakeTaskSavedIndexes, nil, commonpb.IndexState_Finished))
		assert.Nil(t, scheduler.Enqueue(tasks[len(tasks)-1]))
	}
	failTask := newTask(fakeTaskSavedIndexes, nil, commonpb.IndexState_Finished)
	failTask.Ctx().(*stagectx).mimeTimeout = true
	err := scheduler.Enqueue(failTask)
	assert.Error(t, err)
	failTask.Reset()

	scheduler.Start()
	_taskwg.Wait()
	scheduler.Close()
	scheduler.wg.Wait()
	for _, task := range tasks {
		assert.Equal(t, task.GetState(), commonpb.IndexState_Finished)
	}
}
