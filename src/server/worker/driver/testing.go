package driver

import (
	"context"
	"path"
	"time"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/types"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/pps"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsdb"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	"github.com/pachyderm/pachyderm/src/server/worker/common"
	"github.com/pachyderm/pachyderm/src/server/worker/logs"
)

// MockOptions is a basic data struct containing options used by the MockDriver
type MockOptions struct {
	NumWorkers   int
	EtcdPrefix   string
	PipelineInfo *pps.PipelineInfo
}

// MockDriver is an implementation of the Driver interface for use by tests.
// Complicated operations are short-circuited, but etcd operations should still
// work through this.
type MockDriver struct {
	ctx        context.Context
	options    *MockOptions
	etcdClient *etcd.Client
}

// Not used - forces a compile-time error in this file if MockDriver does not
// implement Driver
var _ Driver = &MockDriver{}

// NewMockDriver constructs a MockDriver using the specified fields.
func NewMockDriver(etcdClient *etcd.Client, userOptions *MockOptions) *MockDriver {
	options := &MockOptions{}
	*options = *userOptions

	if options.NumWorkers == 0 {
		options.NumWorkers = 1
	}

	return &MockDriver{
		ctx:        context.Background(),
		options:    options,
		etcdClient: etcdClient,
	}
}

// WithCtx does nothing aside from cloning the current MockDriver since there
// is no pachClient configured.
func (md *MockDriver) WithCtx(ctx context.Context) Driver {
	result := &MockDriver{}
	*result = *md
	result.ctx = ctx
	return result
}

// Jobs returns a collection for the PPS jobs data in etcd
func (md *MockDriver) Jobs() col.Collection {
	return ppsdb.Jobs(md.etcdClient, md.options.EtcdPrefix)
}

// Pipelines returns a collection for the PPS Pipelines data in etcd
func (md *MockDriver) Pipelines() col.Collection {
	return ppsdb.Pipelines(md.etcdClient, md.options.EtcdPrefix)
}

// Plans returns a collection for the PPS plans data in etcd
func (md *MockDriver) Plans() col.Collection {
	return col.NewCollection(md.etcdClient, path.Join(md.options.EtcdPrefix, planPrefix), nil, &common.Plan{}, nil, nil)
}

// Shards returns a collection for the PPS shards data in etcd
func (md *MockDriver) Shards() col.Collection {
	return col.NewCollection(md.etcdClient, path.Join(md.options.EtcdPrefix, shardPrefix), nil, &common.ShardInfo{}, nil, nil)
}

// Chunks returns a collection for the PPS chunks data in etcd
func (md *MockDriver) Chunks(jobID string) col.Collection {
	return col.NewCollection(md.etcdClient, path.Join(md.options.EtcdPrefix, chunkPrefix, jobID), nil, &common.ChunkState{}, nil, nil)
}

// Merges returns a collection for the PPS merges data in etcd
func (md *MockDriver) Merges(jobID string) col.Collection {
	return col.NewCollection(md.etcdClient, path.Join(md.options.EtcdPrefix, mergePrefix, jobID), nil, &common.MergeState{}, nil, nil)
}

func (md *MockDriver) PipelineInfo() *pps.PipelineInfo {
	return md.options.PipelineInfo
}

// InputDir returns the path used to hold the input filesets.  Inherit and
// shadow this if you want to actually load data somewhere (make sure that
// this is unique so that tests don't collide).
func (md *MockDriver) InputDir() string {
	return "/pfs"
}

// PachClient returns the pachd API client for the driver.  This is always
// `nil` for a MockDriver, but you can inherit and shadow this if you want
// some other value.
func (md *MockDriver) PachClient() *client.APIClient {
	return nil
}

// GetExpectedNumWorkers returns the configured number of workers
func (md *MockDriver) GetExpectedNumWorkers() (int, error) {
	return md.options.NumWorkers, nil
}

// WithData doesn't do anything except call the given callback.  Inherit and
// shadow this if you actually want to load some data onto the filesystem.
// Make sure to implement this in terms of the `InputDir` method.
func (md *MockDriver) WithData(
	data []*common.Input,
	inputTree *hashtree.Ordered,
	logger logs.TaggedLogger,
	cb func(*pps.ProcessStats) error,
) (*pps.ProcessStats, error) {
	stats := &pps.ProcessStats{}
	err := cb(stats)
	return stats, err
}

// RunUserCode does nothing.  Inherit and shadow this if you actually want to
// do something for user code
func (md *MockDriver) RunUserCode(logs.TaggedLogger, []string, *pps.ProcessStats, *types.Duration) error {
	return nil
}

// RunUserErrorHandlingCode does nothing.  Inherit and shadow this if you
// actually want to do something for user error-handling code
func (md *MockDriver) RunUserErrorHandlingCode(logs.TaggedLogger, []string, *pps.ProcessStats, *types.Duration) error {
	return nil
}

// DeleteJob will delete the given job entry from etcd.
func (md *MockDriver) DeleteJob(stm col.STM, jobPtr *pps.EtcdJobInfo) error {
	// The dummy version doesn't bother keeping JobCounts updated properly
	return md.Jobs().ReadWrite(stm).Delete(jobPtr.Job.ID)
}

// UpdateJobState will update the given job's state in etcd.
func (md *MockDriver) UpdateJobState(jobID string, state pps.JobState, reason string) error {
	// The dummy version doesn't bother with stats commits
	_, err := md.NewSTM(func(stm col.STM) error {
		jobPtr := &pps.EtcdJobInfo{}
		if err := md.Jobs().ReadWrite(stm).Get(jobID, jobPtr); err != nil {
			return err
		}
		return ppsutil.UpdateJobState(md.Pipelines().ReadWrite(stm), md.Jobs().ReadWrite(stm), jobPtr, state, reason)
	})
	return err
}

// ReportUploadStats does nothing.
func (md *MockDriver) ReportUploadStats(time.Time, *pps.ProcessStats, logs.TaggedLogger) {
	return
}

// NewSTM calls the given callback under a new STM using the configured etcd
// client.
func (md *MockDriver) NewSTM(cb func(col.STM) error) (*etcd.TxnResponse, error) {
	return col.NewSTM(md.ctx, md.etcdClient, cb)
}

// MockKubeWrapper is an alternate implementation of the KubeWrapper interface
// for use with tests.
type MockKubeWrapper struct{}

// NewMockKubeWrapper constructs a MockKubeWrapper for use with testing drivers
// without a kubeClient dependency.
func NewMockKubeWrapper() KubeWrapper {
	return &MockKubeWrapper{}
}

// GetExpectedNumWorkers returns the number of workers the pipeline should be using.
// Inherit and shadow this if you want anything other than 1.
func (mkw *MockKubeWrapper) GetExpectedNumWorkers(*pps.ParallelismSpec) (int, error) {
	return 1, nil
}
