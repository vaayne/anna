package dockerclient

import (
	"context"
	"errors"
	"io"
	"iter"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/containerd/errdefs"
	image "github.com/moby/moby/api/types/image"
	jsonstream "github.com/moby/moby/api/types/jsonstream"
	mobyclient "github.com/moby/moby/client"
)

type lifecycleAPI struct {
	API
	inspectFn func(context.Context, string) (mobyclient.ImageInspectResult, error)
	pullFn    func(context.Context, string) (mobyclient.ImagePullResponse, error)
	createFn  func(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error)
	startFn   func(context.Context, string) (mobyclient.ContainerStartResult, error)
	removeFn  func(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

func (f *lifecycleAPI) ImageInspect(ctx context.Context, image string, _ ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error) {
	if f.inspectFn != nil {
		return f.inspectFn(ctx, image)
	}
	return mobyclient.ImageInspectResult{}, nil
}

func (f *lifecycleAPI) ImagePull(ctx context.Context, image string, _ mobyclient.ImagePullOptions) (mobyclient.ImagePullResponse, error) {
	if f.pullFn != nil {
		return f.pullFn(ctx, image)
	}
	return lifecyclePullResponse{}, nil
}

func (f *lifecycleAPI) ContainerCreate(ctx context.Context, opts mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
	if f.createFn != nil {
		return f.createFn(ctx, opts)
	}
	return mobyclient.ContainerCreateResult{ID: "created-id"}, nil
}

func (f *lifecycleAPI) ContainerStart(ctx context.Context, id string, opts mobyclient.ContainerStartOptions) (mobyclient.ContainerStartResult, error) {
	if f.startFn != nil {
		return f.startFn(ctx, id)
	}
	return mobyclient.ContainerStartResult{}, nil
}

func (f *lifecycleAPI) ContainerRemove(ctx context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
	if f.removeFn != nil {
		return f.removeFn(ctx, id, opts)
	}
	return mobyclient.ContainerRemoveResult{}, nil
}

type lifecyclePullResponse struct{}

func (lifecyclePullResponse) Read([]byte) (int, error) { return 0, io.EOF }
func (lifecyclePullResponse) Close() error             { return nil }
func (lifecyclePullResponse) Wait(context.Context) error {
	return nil
}

func (lifecyclePullResponse) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(yield func(jsonstream.Message, error) bool) {}
}

func TestCreateAndStartRemovesContainerWhenStartFails(t *testing.T) {
	startErr := errors.New("boom")
	var removedID string
	var removedForce bool
	api := &lifecycleAPI{
		startFn: func(context.Context, string) (mobyclient.ContainerStartResult, error) {
			return mobyclient.ContainerStartResult{}, startErr
		},
		removeFn: func(_ context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
			removedID = id
			removedForce = opts.Force
			return mobyclient.ContainerRemoveResult{}, nil
		},
	}
	client := NewWithAPI(api)

	id, err := client.CreateAndStart(context.Background(), CreateOptions{Image: "start-fails:latest", Name: "test"})
	if err == nil {
		t.Fatal("expected start error")
	}
	if id != "" {
		t.Fatalf("id = %q, want empty on start failure", id)
	}
	if removedID != "created-id" || !removedForce {
		t.Fatalf("remove = (%q, force=%v), want created-id force=true", removedID, removedForce)
	}
}

func TestCreateAndStartRejectsDaemonWarningsAndRemovesContainer(t *testing.T) {
	var started bool
	var removedID string
	api := &lifecycleAPI{
		createFn: func(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
			return mobyclient.ContainerCreateResult{ID: "warning-container", Warnings: []string{"CPU limitation discarded"}}, nil
		},
		startFn: func(context.Context, string) (mobyclient.ContainerStartResult, error) {
			started = true
			return mobyclient.ContainerStartResult{}, nil
		},
		removeFn: func(_ context.Context, id string, _ mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
			removedID = id
			return mobyclient.ContainerRemoveResult{}, nil
		},
	}
	client := NewWithAPI(api)

	_, err := client.CreateAndStart(context.Background(), CreateOptions{Image: "warning:latest", Name: "test"})
	if err == nil || !strings.Contains(err.Error(), "CPU limitation discarded") {
		t.Fatalf("CreateAndStart warning error = %v", err)
	}
	if started {
		t.Fatal("container with daemon warnings was started")
	}
	if removedID != "warning-container" {
		t.Fatalf("removed container = %q, want warning-container", removedID)
	}
}

func TestCreateAndStartRemovesContainerWithFreshContextWhenStartCancelsCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cleanupCalled bool
	var cleanupErr error
	api := &lifecycleAPI{
		startFn: func(context.Context, string) (mobyclient.ContainerStartResult, error) {
			cancel()
			return mobyclient.ContainerStartResult{}, context.Canceled
		},
		removeFn: func(ctx context.Context, id string, opts mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error) {
			cleanupCalled = true
			cleanupErr = ctx.Err()
			if id != "created-id" || !opts.Force {
				t.Fatalf("remove = (%q, force=%v), want created-id force=true", id, opts.Force)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("cleanup context should have a deadline")
			}
			return mobyclient.ContainerRemoveResult{}, nil
		},
	}
	client := NewWithAPI(api)

	id, err := client.CreateAndStart(ctx, CreateOptions{Image: "start-cancels:latest", Name: "test"})
	if err == nil {
		t.Fatal("expected start error")
	}
	if id != "" {
		t.Fatalf("id = %q, want empty on start failure", id)
	}
	if !cleanupCalled {
		t.Fatal("expected cleanup remove to run")
	}
	if cleanupErr != nil {
		t.Fatalf("cleanup context err = %v, want nil", cleanupErr)
	}
}

func TestCreateAndStartInvalidatesImageMemoAndRetriesCreateOnNotFound(t *testing.T) {
	const image = "pruned:latest"
	var createCalls atomic.Int32
	var inspectCalls atomic.Int32
	var pullCalls atomic.Int32
	api := &lifecycleAPI{
		inspectFn: func(context.Context, string) (mobyclient.ImageInspectResult, error) {
			inspectCalls.Add(1)
			return mobyclient.ImageInspectResult{}, errdefs.ErrNotFound
		},
		pullFn: func(context.Context, string) (mobyclient.ImagePullResponse, error) {
			pullCalls.Add(1)
			return lifecyclePullResponse{}, nil
		},
		createFn: func(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
			if createCalls.Add(1) == 1 {
				return mobyclient.ContainerCreateResult{}, errdefs.ErrNotFound
			}
			return mobyclient.ContainerCreateResult{ID: "created-after-repull"}, nil
		},
	}
	client := NewWithAPI(api)

	id, err := client.CreateAndStart(context.Background(), CreateOptions{Image: image, Name: "test"})
	if err != nil {
		t.Fatalf("CreateAndStart: %v", err)
	}
	if id != "created-after-repull" {
		t.Fatalf("id = %q, want created-after-repull", id)
	}
	if got := createCalls.Load(); got != 2 {
		t.Fatalf("create calls = %d, want 2", got)
	}
	if got := inspectCalls.Load(); got != 2 {
		t.Fatalf("inspect calls = %d, want 2", got)
	}
	if got := pullCalls.Load(); got != 2 {
		t.Fatalf("pull calls = %d, want 2", got)
	}
	if !client.isImageReady(image) {
		t.Fatal("image should be memoized ready after retry pull")
	}
}

func TestCreateAndStartTypesImageFailureAfterConcurrentPrune(t *testing.T) {
	const image = "pruned:latest"
	api := &lifecycleAPI{
		inspectFn: func(context.Context, string) (mobyclient.ImageInspectResult, error) {
			return mobyclient.ImageInspectResult{}, errdefs.ErrNotFound
		},
		pullFn: func(context.Context, string) (mobyclient.ImagePullResponse, error) {
			return nil, errors.New("pull unavailable")
		},
		createFn: func(context.Context, mobyclient.ContainerCreateOptions) (mobyclient.ContainerCreateResult, error) {
			return mobyclient.ContainerCreateResult{}, errdefs.ErrNotFound
		},
	}
	client := NewWithAPI(api)
	client.imageReady[image] = struct{}{}

	_, err := client.CreateAndStart(context.Background(), CreateOptions{Image: image, Name: "test"})
	var imageErr *ImageUnavailableError
	if !errors.As(err, &imageErr) {
		t.Fatalf("CreateAndStart error = %v, want ImageUnavailableError", err)
	}
}

func TestEnsureImageReadySingleflightsConcurrentInspect(t *testing.T) {
	var inspectCalls atomic.Int32
	unblock := make(chan struct{})
	api := &lifecycleAPI{
		inspectFn: func(ctx context.Context, image string) (mobyclient.ImageInspectResult, error) {
			inspectCalls.Add(1)
			select {
			case <-ctx.Done():
				return mobyclient.ImageInspectResult{}, ctx.Err()
			case <-unblock:
				return mobyclient.ImageInspectResult{}, nil
			}
		},
	}
	client := NewWithAPI(api)

	const callers = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			<-start
			errs <- client.EnsureImageReady(context.Background(), "singleflight-image:latest", "test")
		})
	}
	close(start)
	for inspectCalls.Load() == 0 {
	}
	close(unblock)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureImageReady: %v", err)
		}
	}
	if got := inspectCalls.Load(); got != 1 {
		t.Fatalf("inspect calls = %d, want 1", got)
	}
}

func TestImageIDUsesContentAddressedInspectID(t *testing.T) {
	client := NewWithAPI(&lifecycleAPI{inspectFn: func(context.Context, string) (mobyclient.ImageInspectResult, error) {
		return mobyclient.ImageInspectResult{InspectResponse: image.InspectResponse{ID: "sha256:resolved"}}, nil
	}})
	got, err := client.ImageID(context.Background(), "stella:tag")
	if err != nil {
		t.Fatalf("ImageID: %v", err)
	}
	if got != "sha256:resolved" {
		t.Fatalf("ImageID = %q, want resolved digest", got)
	}
}
