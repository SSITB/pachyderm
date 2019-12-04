package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/types"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/go-playground/webhooks.v5/github"
	"gopkg.in/src-d/go-git.v4"
	gitPlumbing "gopkg.in/src-d/go-git.v4/plumbing"

	"github.com/pachyderm/pachyderm/src/client"
	"github.com/pachyderm/pachyderm/src/client/enterprise"
	"github.com/pachyderm/pachyderm/src/client/pfs"
	"github.com/pachyderm/pachyderm/src/client/pkg/grpcutil"
	"github.com/pachyderm/pachyderm/src/client/pps"
	col "github.com/pachyderm/pachyderm/src/server/pkg/collection"
	"github.com/pachyderm/pachyderm/src/server/pkg/exec"
	"github.com/pachyderm/pachyderm/src/server/pkg/hashtree"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsdb"
	"github.com/pachyderm/pachyderm/src/server/pkg/ppsutil"
	filesync "github.com/pachyderm/pachyderm/src/server/pkg/sync"
	"github.com/pachyderm/pachyderm/src/server/pkg/uuid"
	"github.com/pachyderm/pachyderm/src/server/worker/common"
	"github.com/pachyderm/pachyderm/src/server/worker/logs"
	"github.com/pachyderm/pachyderm/src/server/worker/stats"
)

const (
	// The maximum number of concurrent download/upload operations
	concurrency = 100

	chunkPrefix = "/chunk"
	mergePrefix = "/merge"
	planPrefix  = "/plan"
	shardPrefix = "/shard"
)

var (
	errSpecialFile = errors.New("cannot upload special file")
)

// Driver provides an interface for common functions needed by worker code, and
// captures the relevant objects necessary to provide these functions so that
// users do not need to keep track of as many variables.  In addition, this
// interface can be used to mock out external calls to make unit-testing
// simpler.
type Driver interface {
	Jobs() col.Collection
	Pipelines() col.Collection
	Plans() col.Collection
	Shards() col.Collection
	Chunks(jobID string) col.Collection
	Merges(jobID string) col.Collection

	PipelineInfo() *pps.PipelineInfo

	// Returns the path that will contain the input filesets for the job
	InputDir() string

	// Returns the pachd API client for the driver
	PachClient() *client.APIClient

	// Returns the number of workers to be used based on what can be determined from kubernetes
	GetExpectedNumWorkers() (int, error)

	// WithCtx clones the current driver and applies the context to its
	// pachClient. The pachClient context will be used for other blocking
	// operations as well.
	WithCtx(context.Context) Driver

	// WithData prepares the current node the code is running on to run a piece
	// of user code by downloading the specified data, and cleans up afterwards.
	WithData([]*common.Input, *hashtree.Ordered, logs.TaggedLogger, func(*pps.ProcessStats) error) (*pps.ProcessStats, error)

	// RunUserCode runs the pipeline's configured code
	RunUserCode(logs.TaggedLogger, []string, *pps.ProcessStats, *types.Duration) error

	// RunUserErrorHandlingCode runs the pipeline's configured error handling code
	RunUserErrorHandlingCode(logs.TaggedLogger, []string, *pps.ProcessStats, *types.Duration) error

	// TODO: provide a more generic interface for modifying jobs/plans/etc, and
	// some quality-of-life functions for common operations.
	DeleteJob(col.STM, *pps.EtcdJobInfo) error
	UpdateJobState(string, pps.JobState, string) error

	// TODO: figure out how to not expose this
	ReportUploadStats(time.Time, *pps.ProcessStats, logs.TaggedLogger)

	// TODO: figure out how to not expose this - currently only used for a few
	// operations in the map spawner
	NewSTM(func(col.STM) error) (*etcd.TxnResponse, error)
}

type driver struct {
	pipelineInfo *pps.PipelineInfo
	pachClient   *client.APIClient
	kubeWrapper  KubeWrapper
	etcdClient   *etcd.Client
	etcdPrefix   string

	jobs col.Collection

	pipelines col.Collection

	plans col.Collection

	// Stores available filesystem shards for a pipeline, workers will claim these
	shards col.Collection

	// User and group IDs used for running user code, determined in the constructor
	uid *uint32
	gid *uint32

	// We only export application statistics if enterprise is enabled
	exportStats bool

	// The directory to store input data - this is typically static but can be
	// overridden by tests
	inputDir string
}

// NewDriver constructs a Driver object using the given clients and pipeline
// settings.  It makes blocking calls to determine the user/group to use with
// the user code on the current worker node, as well as determining if
// enterprise features are activated (for exporting stats).
func NewDriver(
	pipelineInfo *pps.PipelineInfo,
	pachClient *client.APIClient,
	kubeWrapper KubeWrapper,
	etcdClient *etcd.Client,
	etcdPrefix string,
) (Driver, error) {
	result := &driver{
		pipelineInfo: pipelineInfo,
		pachClient:   pachClient,
		kubeWrapper:  kubeWrapper,
		etcdClient:   etcdClient,
		etcdPrefix:   etcdPrefix,
		jobs:         ppsdb.Jobs(etcdClient, etcdPrefix),
		pipelines:    ppsdb.Pipelines(etcdClient, etcdPrefix),
		shards:       col.NewCollection(etcdClient, path.Join(etcdPrefix, shardPrefix, pipelineInfo.Pipeline.Name), nil, &common.ShardInfo{}, nil, nil),
		plans:        col.NewCollection(etcdClient, path.Join(etcdPrefix, planPrefix), nil, &common.Plan{}, nil, nil),
		inputDir:     client.PPSInputPrefix,
	}

	if pipelineInfo.Transform.User != "" {
		user, err := lookupDockerUser(pipelineInfo.Transform.User)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		// If `user` is `nil`, `uid` and `gid` will get set, and we won't
		// customize the user that executes the worker process.
		if user != nil { // user is nil when os.IsNotExist(err) is true in which case we use root
			uid, err := strconv.ParseUint(user.Uid, 10, 32)
			if err != nil {
				return nil, err
			}
			uid32 := uint32(uid)
			result.uid = &uid32
			gid, err := strconv.ParseUint(user.Gid, 10, 32)
			if err != nil {
				return nil, err
			}
			gid32 := uint32(gid)
			result.gid = &gid32
		}
	}

	if resp, err := pachClient.Enterprise.GetState(context.Background(), &enterprise.GetStateRequest{}); err != nil {
		logs.NewStatlessLogger(pipelineInfo).Logf("failed to get enterprise state with error: %v\n", err)
	} else {
		result.exportStats = resp.State == enterprise.State_ACTIVE
	}

	return result, nil
}

// lookupDockerUser looks up users given the argument to a Dockerfile USER directive.
// According to Docker's docs this directive looks like:
// USER <user>[:<group>] or
// USER <UID>[:<GID>]
func lookupDockerUser(userArg string) (_ *user.User, retErr error) {
	userParts := strings.Split(userArg, ":")
	userOrUID := userParts[0]
	groupOrGID := ""
	if len(userParts) > 1 {
		groupOrGID = userParts[1]
	}
	passwd, err := os.Open("/etc/passwd")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := passwd.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	scanner := bufio.NewScanner(passwd)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if parts[0] == userOrUID || parts[2] == userOrUID {
			result := &user.User{
				Username: parts[0],
				Uid:      parts[2],
				Gid:      parts[3],
				Name:     parts[4],
				HomeDir:  parts[5],
			}
			if groupOrGID != "" {
				if parts[0] == userOrUID {
					// groupOrGid is a group
					group, err := lookupGroup(groupOrGID)
					if err != nil {
						return nil, err
					}
					result.Gid = group.Gid
				} else {
					// groupOrGid is a gid
					result.Gid = groupOrGID
				}
			}
			return result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		log.Fatal(err)
	}
	return nil, fmt.Errorf("user %s not found", userArg)
}

func lookupGroup(group string) (_ *user.Group, retErr error) {
	groupFile, err := os.Open("/etc/group")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := groupFile.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	scanner := bufio.NewScanner(groupFile)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if parts[0] == group {
			return &user.Group{
				Gid:  parts[2],
				Name: parts[0],
			}, nil
		}
	}
	return nil, fmt.Errorf("group %s not found", group)
}

func (d *driver) WithCtx(ctx context.Context) Driver {
	result := &driver{}
	*result = *d
	result.pachClient = result.pachClient.WithCtx(ctx)
	return result
}

func (d *driver) Jobs() col.Collection {
	return d.jobs
}

func (d *driver) Pipelines() col.Collection {
	return d.pipelines
}

func (d *driver) Plans() col.Collection {
	return d.plans
}

func (d *driver) Shards() col.Collection {
	return d.shards
}

func (d *driver) Chunks(jobID string) col.Collection {
	return col.NewCollection(d.etcdClient, path.Join(d.etcdPrefix, chunkPrefix, jobID), nil, &common.ChunkState{}, nil, nil)
}

func (d *driver) Merges(jobID string) col.Collection {
	return col.NewCollection(d.etcdClient, path.Join(d.etcdPrefix, mergePrefix, jobID), nil, &common.MergeState{}, nil, nil)
}

func (d *driver) GetExpectedNumWorkers() (int, error) {
	return d.kubeWrapper.GetExpectedNumWorkers(d.pipelineInfo.ParallelismSpec)
}

func (d *driver) PipelineInfo() *pps.PipelineInfo {
	return d.pipelineInfo
}

func (d *driver) InputDir() string {
	return d.inputDir
}

func (d *driver) PachClient() *client.APIClient {
	return d.pachClient
}

func (d *driver) NewSTM(cb func(col.STM) error) (*etcd.TxnResponse, error) {
	return col.NewSTM(d.pachClient.Ctx(), d.etcdClient, cb)
}

func (d *driver) WithData(
	data []*common.Input,
	inputTree *hashtree.Ordered,
	logger logs.TaggedLogger,
	cb func(*pps.ProcessStats) error,
) (retStats *pps.ProcessStats, retErr error) {
	puller := filesync.NewPuller()
	stats := &pps.ProcessStats{}

	// Download input data into a temporary directory
	// This can be interrupted via the pachClient using driver.WithCtx
	dir, err := d.downloadData(logger, data, puller, stats, inputTree)
	// We run these cleanup functions no matter what, so that if
	// downloadData partially succeeded, we still clean up the resources.
	defer func() {
		if err := os.RemoveAll(dir); err != nil && retErr == nil {
			retErr = err
		}
	}()
	// It's important that we run puller.CleanUp before os.RemoveAll,
	// because otherwise puller.Cleanup might try to open pipes that have
	// been deleted.
	defer func() {
		if _, err := puller.CleanUp(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	if err != nil {
		return nil, fmt.Errorf("error downloadData: %v", err)
	}
	if err := os.MkdirAll(d.inputDir, 0777); err != nil {
		return nil, err
	}
	if err := d.linkData(data, dir); err != nil {
		return nil, fmt.Errorf("error linkData: %v", err)
	}
	defer func() {
		if err := d.unlinkData(data); err != nil && retErr == nil {
			retErr = fmt.Errorf("error unlinkData: %v", err)
		}
	}()
	// If the pipeline spec set a custom user to execute the process, make sure
	// the input directory and its content are owned by it
	if d.uid != nil && d.gid != nil {
		filepath.Walk(d.inputDir, func(name string, info os.FileInfo, err error) error {
			if err == nil {
				err = os.Chown(name, int(*d.uid), int(*d.gid))
			}
			return err
		})
	}

	if err := cb(stats); err != nil {
		return nil, err
	}

	// CleanUp is idempotent so we can call it however many times we want.
	// The reason we are calling it here is that the puller could've
	// encountered an error as it was lazily loading files, in which case
	// the output might be invalid since as far as the user's code is
	// concerned, they might've just seen an empty or partially completed
	// file.
	// TODO: do we really need two puller.CleanUps?
	downSize, err := puller.CleanUp()
	if err != nil {
		logger.Logf("puller encountered an error while cleaning up: %+v", err)
		return nil, err
	}

	atomic.AddUint64(&stats.DownloadBytes, uint64(downSize))
	d.reportDownloadSizeStats(float64(downSize), logger)
	return stats, nil
}

func (d *driver) downloadData(
	logger logs.TaggedLogger,
	inputs []*common.Input,
	puller *filesync.Puller,
	stats *pps.ProcessStats,
	statsTree *hashtree.Ordered,
) (_ string, retErr error) {
	defer d.reportDownloadTimeStats(time.Now(), stats, logger)
	logger.Logf("starting to download data")
	defer func(start time.Time) {
		if retErr != nil {
			logger.Logf("errored downloading data after %v: %v", time.Since(start), retErr)
		} else {
			logger.Logf("finished downloading data after %v", time.Since(start))
		}
	}(time.Now())

	// The scratch space is where Pachyderm stores downloaded and output data, which is
	// then symlinked into place for the pipeline.
	scratchPath := filepath.Join(d.InputDir(), client.PPSScratchSpace, uuid.NewWithoutDashes())
	outPath := filepath.Join(scratchPath, "out")
	if d.pipelineInfo.Spout != nil {
		// Spouts need to create a named pipe at /pfs/out
		if err := os.MkdirAll(filepath.Dir(outPath), 0700); err != nil {
			return "", fmt.Errorf("mkdirall :%v", err)
		}
		// Fifos don't exist on windows (where we may run this code in tests), so
		// this is broken out into a file
		if err := createSpoutFifo(outPath); err != nil {
			return "", fmt.Errorf("mkfifo :%v", err)
		}
	} else {
		// Create output directory (typically /pfs/out)
		if err := os.MkdirAll(outPath, 0777); err != nil {
			return "", err
		}
	}
	for _, input := range inputs {
		if input.GitURL != "" {
			if err := d.downloadGitData(scratchPath, input); err != nil {
				return "", err
			}
			continue
		}
		file := input.FileInfo.File
		fullInputPath := filepath.Join(scratchPath, input.Name, file.Path)
		var statsRoot string
		if statsTree != nil {
			statsRoot = path.Join(input.Name, file.Path)
			parent, _ := path.Split(statsRoot)
			statsTree.MkdirAll(parent)
		}
		if err := puller.Pull(
			d.pachClient,
			fullInputPath,
			file.Commit.Repo.Name,
			file.Commit.ID,
			file.Path,
			input.Lazy,
			input.EmptyFiles,
			concurrency,
			statsTree,
			statsRoot,
		); err != nil {
			return "", err
		}
	}
	return scratchPath, nil
}

func (d *driver) downloadGitData(scratchPath string, input *common.Input) error {
	file := input.FileInfo.File

	var rawJSON bytes.Buffer
	err := d.pachClient.GetFile(file.Commit.Repo.Name, file.Commit.ID, file.Path, 0, 0, &rawJSON)
	if err != nil {
		return err
	}

	var payload github.PushPayload
	err = json.Unmarshal(rawJSON.Bytes(), &payload)
	if err != nil {
		return err
	}

	if payload.Repository.CloneURL == "" {
		return fmt.Errorf("Git hook payload does not specify the upstream URL")
	} else if payload.Ref == "" {
		return fmt.Errorf("Git hook payload does not specify the updated ref")
	} else if payload.After == "" {
		return fmt.Errorf("Git hook payload does not specify the commit SHA")
	}

	// Clone checks out a reference, not a SHA. Github does not support fetching
	// an individual SHA.
	remoteURL := payload.Repository.CloneURL
	gitRepo, err := git.PlainCloneContext(
		d.pachClient.Ctx(),
		filepath.Join(scratchPath, input.Name),
		false,
		&git.CloneOptions{
			URL:           remoteURL,
			SingleBranch:  true,
			ReferenceName: gitPlumbing.ReferenceName(payload.Ref),
		},
	)
	if err != nil {
		return fmt.Errorf("error fetching repo %v with ref %v from URL %v: %v", input.Name, payload.Ref, remoteURL, err)
	}

	wt, err := gitRepo.Worktree()
	if err != nil {
		return err
	}

	sha := payload.After
	err = wt.Checkout(&git.CheckoutOptions{Hash: gitPlumbing.NewHash(sha)})
	if err != nil {
		return fmt.Errorf("error checking out SHA %v for repo %v: %v", sha, input.Name, err)
	}

	// go-git will silently fail to checkout an invalid SHA and leave the HEAD at
	// the selected ref. Verify that we are now on the correct SHA
	rev, err := gitRepo.ResolveRevision("HEAD")
	if err != nil {
		return fmt.Errorf("failed to inspect HEAD SHA for repo %v: %v", input.Name, err)
	} else if rev.String() != sha {
		return fmt.Errorf("could not find SHA %v for repo %v", sha, input.Name)
	}

	return nil
}

func (d *driver) unlinkData(inputs []*common.Input) error {
	entries, err := ioutil.ReadDir(d.inputDir)
	if err != nil {
		return fmt.Errorf("ioutil.ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == client.PPSScratchSpace {
			continue // don't delete scratch space
		}
		if err := os.RemoveAll(filepath.Join(d.inputDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Run user code and return the combined output of stdout and stderr.
func (d *driver) RunUserCode(
	logger logs.TaggedLogger,
	environ []string,
	procStats *pps.ProcessStats,
	rawDatumTimeout *types.Duration,
) (retErr error) {
	ctx := d.pachClient.Ctx()
	d.reportUserCodeStats(logger)
	defer func(start time.Time) { d.reportDeferredUserCodeStats(retErr, start, procStats, logger) }(time.Now())
	logger.Logf("beginning to run user code")
	defer func(start time.Time) {
		if retErr != nil {
			logger.Logf("errored running user code after %v: %v", time.Since(start), retErr)
		} else {
			logger.Logf("finished running user code after %v", time.Since(start))
		}
	}(time.Now())
	if rawDatumTimeout != nil {
		datumTimeout, err := types.DurationFromProto(rawDatumTimeout)
		if err != nil {
			return err
		}
		datumTimeoutCtx, cancel := context.WithTimeout(ctx, datumTimeout)
		defer cancel()
		ctx = datumTimeoutCtx
	}

	if len(d.pipelineInfo.Transform.Cmd) == 0 {
		return fmt.Errorf("invalid pipeline transform, no command specified")
	}

	// Run user code
	cmd := exec.CommandContext(ctx, d.pipelineInfo.Transform.Cmd[0], d.pipelineInfo.Transform.Cmd[1:]...)
	if d.pipelineInfo.Transform.Stdin != nil {
		cmd.Stdin = strings.NewReader(strings.Join(d.pipelineInfo.Transform.Stdin, "\n") + "\n")
	}
	cmd.Stdout = logger.WithUserCode()
	cmd.Stderr = logger.WithUserCode()
	cmd.Env = environ
	if d.uid != nil && d.gid != nil {
		cmd.SysProcAttr = makeCmdCredentials(*d.uid, *d.gid)
	}
	cmd.Dir = d.pipelineInfo.Transform.WorkingDir
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error cmd.Start: %v", err)
	}
	// A context with a deadline will successfully cancel/kill
	// the running process (minus zombies)
	state, err := cmd.Process.Wait()
	if err != nil {
		return fmt.Errorf("error cmd.Wait: %v", err)
	}
	if common.IsDone(ctx) {
		if err = ctx.Err(); err != nil {
			return err
		}
	}

	// Because of this issue: https://github.com/golang/go/issues/18874
	// We forked os/exec so that we can call just the part of cmd.Wait() that
	// happens after blocking on the process. Unfortunately calling
	// cmd.Process.Wait() then cmd.Wait() will produce an error. So instead we
	// close the IO using this helper
	err = cmd.WaitIO(state, err)
	// We ignore broken pipe errors, these occur very occasionally if a user
	// specifies Stdin but their process doesn't actually read everything from
	// Stdin. This is a fairly common thing to do, bash by default ignores
	// broken pipe errors.
	if err != nil && !strings.Contains(err.Error(), "broken pipe") {
		// (if err is an acceptable return code, don't return err)
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				for _, returnCode := range d.pipelineInfo.Transform.AcceptReturnCode {
					if int(returnCode) == status.ExitStatus() {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("error cmd.WaitIO: %v", err)
	}
	return nil
}

// Run user error code and return the combined output of stdout and stderr.
func (d *driver) RunUserErrorHandlingCode(logger logs.TaggedLogger, environ []string, procStats *pps.ProcessStats, rawDatumTimeout *types.Duration) (retErr error) {
	ctx := d.pachClient.Ctx()
	logger.Logf("beginning to run user error handling code")
	defer func(start time.Time) {
		if retErr != nil {
			logger.Logf("errored running user error handling code after %v: %v", time.Since(start), retErr)
		} else {
			logger.Logf("finished running user error handling code after %v", time.Since(start))
		}
	}(time.Now())

	cmd := exec.CommandContext(ctx, d.pipelineInfo.Transform.ErrCmd[0], d.pipelineInfo.Transform.ErrCmd[1:]...)
	if d.pipelineInfo.Transform.ErrStdin != nil {
		cmd.Stdin = strings.NewReader(strings.Join(d.pipelineInfo.Transform.ErrStdin, "\n") + "\n")
	}
	cmd.Stdout = logger.WithUserCode()
	cmd.Stderr = logger.WithUserCode()
	cmd.Env = environ
	if d.uid != nil && d.gid != nil {
		cmd.SysProcAttr = makeCmdCredentials(*d.uid, *d.gid)
	}
	cmd.Dir = d.pipelineInfo.Transform.WorkingDir
	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error cmd.Start: %v", err)
	}
	// A context w a deadline will successfully cancel/kill
	// the running process (minus zombies)
	state, err := cmd.Process.Wait()
	if err != nil {
		return fmt.Errorf("error cmd.Wait: %v", err)
	}
	if common.IsDone(ctx) {
		if err = ctx.Err(); err != nil {
			return err
		}
	}
	// Because of this issue: https://github.com/golang/go/issues/18874
	// We forked os/exec so that we can call just the part of cmd.Wait() that
	// happens after blocking on the process. Unfortunately calling
	// cmd.Process.Wait() then cmd.Wait() will produce an error. So instead we
	// close the IO using this helper
	err = cmd.WaitIO(state, err)
	// We ignore broken pipe errors, these occur very occasionally if a user
	// specifies Stdin but their process doesn't actually read everything from
	// Stdin. This is a fairly common thing to do, bash by default ignores
	// broken pipe errors.
	if err != nil && !strings.Contains(err.Error(), "broken pipe") {
		// (if err is an acceptable return code, don't return err)
		if exiterr, ok := err.(*exec.ExitError); ok {
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				for _, returnCode := range d.pipelineInfo.Transform.AcceptReturnCode {
					if int(returnCode) == status.ExitStatus() {
						return nil
					}
				}
			}
		}
		return fmt.Errorf("error cmd.WaitIO: %v", err)
	}
	return nil
}

func (d *driver) UpdateJobState(jobID string, state pps.JobState, reason string) error {
	_, err := d.NewSTM(func(stm col.STM) error {
		jobPtr := &pps.EtcdJobInfo{}
		if err := d.Jobs().ReadWrite(stm).Get(jobID, jobPtr); err != nil {
			return err
		}
		return ppsutil.UpdateJobState(d.Pipelines().ReadWrite(stm), d.Jobs().ReadWrite(stm), jobPtr, state, reason)
	})
	return err
}

// DeleteJob is identical to updateJobState, except that jobPtr points to a job
// that should be deleted rather than marked failed. Jobs may be deleted if
// their output commit is deleted.
func (d *driver) DeleteJob(stm col.STM, jobPtr *pps.EtcdJobInfo) error {
	pipelinePtr := &pps.EtcdPipelineInfo{}
	if err := d.Pipelines().ReadWrite(stm).Update(jobPtr.Pipeline.Name, pipelinePtr, func() error {
		if pipelinePtr.JobCounts == nil {
			pipelinePtr.JobCounts = make(map[int32]int32)
		}
		if pipelinePtr.JobCounts[int32(jobPtr.State)] != 0 {
			pipelinePtr.JobCounts[int32(jobPtr.State)]--
		}
		return nil
	}); err != nil {
		return err
	}
	return d.Jobs().ReadWrite(stm).Delete(jobPtr.Job.ID)
}

func (d *driver) updateCounter(
	stat *prometheus.CounterVec,
	logger logs.TaggedLogger,
	state string,
	cb func(prometheus.Counter),
) {
	labels := []string{d.pipelineInfo.ID, logger.JobID()}
	if state != "" {
		labels = append(labels, state)
	}
	if counter, err := stat.GetMetricWithLabelValues(labels...); err != nil {
		logger.Logf("failed to get counter with labels (%v): %v", labels, err)
	} else {
		cb(counter)
	}
}

func (d *driver) updateHistogram(
	stat *prometheus.HistogramVec,
	logger logs.TaggedLogger,
	state string,
	cb func(prometheus.Observer),
) {
	labels := []string{d.pipelineInfo.ID, logger.JobID()}
	if state != "" {
		labels = append(labels, state)
	}
	if hist, err := stat.GetMetricWithLabelValues(labels...); err != nil {
		logger.Logf("failed to get histogram with labels (%v): %v", labels, err)
	} else {
		cb(hist)
	}
}

func (d *driver) reportUserCodeStats(logger logs.TaggedLogger) {
	if d.exportStats {
		d.updateCounter(stats.DatumCount, logger, "started", func(counter prometheus.Counter) {
			counter.Add(1)
		})
	}
}

func (d *driver) reportDeferredUserCodeStats(
	err error,
	start time.Time,
	procStats *pps.ProcessStats,
	logger logs.TaggedLogger,
) {
	if d.exportStats {
		duration := time.Since(start)
		procStats.ProcessTime = types.DurationProto(duration)

		state := "errored"
		if err == nil {
			state = "finished"
		}

		d.updateCounter(stats.DatumCount, logger, state, func(counter prometheus.Counter) {
			counter.Add(1)
		})
		d.updateHistogram(stats.DatumProcTime, logger, state, func(hist prometheus.Observer) {
			hist.Observe(duration.Seconds())
		})
		d.updateCounter(stats.DatumProcSecondsCount, logger, "", func(counter prometheus.Counter) {
			counter.Add(duration.Seconds())
		})
	}
}

func (d *driver) ReportUploadStats(
	start time.Time,
	procStats *pps.ProcessStats,
	logger logs.TaggedLogger,
) {
	if d.exportStats {
		duration := time.Since(start)
		procStats.UploadTime = types.DurationProto(duration)

		d.updateHistogram(stats.DatumUploadTime, logger, "", func(hist prometheus.Observer) {
			hist.Observe(duration.Seconds())
		})
		d.updateCounter(stats.DatumUploadSecondsCount, logger, "", func(counter prometheus.Counter) {
			counter.Add(duration.Seconds())
		})
		d.updateHistogram(stats.DatumUploadSize, logger, "", func(hist prometheus.Observer) {
			hist.Observe(float64(procStats.UploadBytes))
		})
		d.updateCounter(stats.DatumUploadBytesCount, logger, "", func(counter prometheus.Counter) {
			counter.Add(float64(procStats.UploadBytes))
		})
	}
}

func (d *driver) reportDownloadSizeStats(
	downSize float64,
	logger logs.TaggedLogger,
) {
	if d.exportStats {
		d.updateHistogram(stats.DatumDownloadSize, logger, "", func(hist prometheus.Observer) {
			hist.Observe(downSize)
		})
		d.updateCounter(stats.DatumDownloadBytesCount, logger, "", func(counter prometheus.Counter) {
			counter.Add(downSize)
		})
	}
}

func (d *driver) reportDownloadTimeStats(
	start time.Time,
	procStats *pps.ProcessStats,
	logger logs.TaggedLogger,
) {
	if d.exportStats {
		duration := time.Since(start)
		procStats.DownloadTime = types.DurationProto(duration)

		d.updateHistogram(stats.DatumDownloadTime, logger, "", func(hist prometheus.Observer) {
			hist.Observe(duration.Seconds())
		})
		d.updateCounter(stats.DatumDownloadSecondsCount, logger, "", func(counter prometheus.Counter) {
			counter.Add(duration.Seconds())
		})
	}
}

func (d *driver) UploadOutput(
	tag string,
	logger logs.TaggedLogger,
	inputs []*common.Input,
	stats *pps.ProcessStats,
	statsTree *hashtree.Ordered,
) (retBuffer []byte, retErr error) {
	defer d.ReportUploadStats(time.Now(), stats, logger)
	logger.Logf("starting to upload output")
	defer func(start time.Time) {
		if retErr != nil {
			logger.Logf("errored uploading output after %v: %v", time.Since(start), retErr)
		} else {
			logger.Logf("finished uploading output after %v", time.Since(start))
		}
	}(time.Now())

	// Set up client for writing file data
	putObjsClient, err := d.pachClient.ObjectAPIClient.PutObjects(d.pachClient.Ctx())
	if err != nil {
		return nil, err
	}
	block := &pfs.Block{Hash: uuid.NewWithoutDashes()}
	if err := putObjsClient.Send(&pfs.PutObjectRequest{
		Block: block,
	}); err != nil {
		return nil, err
	}
	outputPath := filepath.Join(d.InputDir(), "out")
	buf := grpcutil.GetBuffer()
	defer grpcutil.PutBuffer(buf)
	var offset uint64
	var tree *hashtree.Ordered

	// Upload all files in output directory
	if err := filepath.Walk(outputPath, func(filePath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !utf8.ValidString(filePath) {
			return fmt.Errorf("file path is not valid utf-8: %s", filePath)
		}
		if filePath == outputPath {
			tree = hashtree.NewOrdered("/")
			return nil
		}
		relPath, err := filepath.Rel(outputPath, filePath)
		if err != nil {
			return err
		}
		// Put directory. Even if the directory is empty, that may be useful to
		// users
		// TODO(msteffen) write a test pipeline that outputs an empty directory and
		// make sure it's preserved
		if info.IsDir() {
			tree.PutDir(relPath)
			if statsTree != nil {
				statsTree.PutDir(relPath)
			}
			return nil
		}
		// Under some circumstances, the user might have copied
		// some pipes from the input directory to the output directory.
		// Reading from these files will result in job blocking.  Thus
		// we preemptively detect if the file is a named pipe.
		if (info.Mode() & os.ModeNamedPipe) > 0 {
			logger.Logf("cannot upload named pipe: %v", relPath)
			return errSpecialFile
		}
		// If the output file is a symlink to an input file, we can skip
		// the uploading.
		if (info.Mode() & os.ModeSymlink) > 0 {
			realPath, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			if strings.HasPrefix(realPath, d.InputDir()) {
				var pathWithInput string
				var err error
				if strings.HasPrefix(realPath, relPath) {
					pathWithInput, err = filepath.Rel(relPath, realPath)
				} else {
					pathWithInput, err = filepath.Rel(d.InputDir(), realPath)
				}
				if err == nil {
					// We can only skip the upload if the real path is
					// under /pfs, meaning that it's a file that already
					// exists in PFS.

					// The name of the input
					inputName := strings.Split(pathWithInput, string(os.PathSeparator))[0]
					var input *common.Input
					for _, i := range inputs {
						if i.Name == inputName {
							input = i
						}
					}
					// this changes realPath from `/pfs/input/...` to `/scratch/<id>/input/...`
					realPath = filepath.Join(relPath, pathWithInput)
					if input != nil {
						return filepath.Walk(realPath, func(filePath string, info os.FileInfo, err error) error {
							if err != nil {
								return err
							}
							rel, err := filepath.Rel(realPath, filePath)
							if err != nil {
								return err
							}
							subRelPath := filepath.Join(relPath, rel)
							// The path of the input file
							pfsPath, err := filepath.Rel(filepath.Join(relPath, input.Name), filePath)
							if err != nil {
								return err
							}
							if info.IsDir() {
								tree.PutDir(subRelPath)
								if statsTree != nil {
									statsTree.PutDir(subRelPath)
								}
								return nil
							}
							fc := input.FileInfo.File.Commit
							fileInfo, err := d.pachClient.InspectFile(fc.Repo.Name, fc.ID, pfsPath)
							if err != nil {
								return err
							}
							var blockRefs []*pfs.BlockRef
							for _, object := range fileInfo.Objects {
								objectInfo, err := d.pachClient.InspectObject(object.Hash)
								if err != nil {
									return err
								}
								blockRefs = append(blockRefs, objectInfo.BlockRef)
							}
							blockRefs = append(blockRefs, fileInfo.BlockRefs...)
							n := &hashtree.FileNodeProto{BlockRefs: blockRefs}
							tree.PutFile(subRelPath, fileInfo.Hash, int64(fileInfo.SizeBytes), n)
							if statsTree != nil {
								statsTree.PutFile(subRelPath, fileInfo.Hash, int64(fileInfo.SizeBytes), n)
							}
							return nil
						})
					}
				}
			}
		}
		// Open local file that is being uploaded
		f, err := os.Open(filePath)
		if err != nil {
			return fmt.Errorf("os.Open(%s): %v", filePath, err)
		}
		defer func() {
			if err := f.Close(); err != nil && retErr == nil {
				retErr = err
			}
		}()
		var size int64
		h := pfs.NewHash()
		r := io.TeeReader(f, h)
		// Write local file to object storage block
		for {
			n, err := r.Read(buf)
			if n == 0 && err != nil {
				if err == io.EOF {
					break
				}
				return err
			}
			if err := putObjsClient.Send(&pfs.PutObjectRequest{
				Value: buf[:n],
			}); err != nil {
				return err
			}
			size += int64(n)
		}
		n := &hashtree.FileNodeProto{
			BlockRefs: []*pfs.BlockRef{
				&pfs.BlockRef{
					Block: block,
					Range: &pfs.ByteRange{
						Lower: offset,
						Upper: offset + uint64(size),
					},
				},
			},
		}
		hash := h.Sum(nil)
		tree.PutFile(relPath, hash, size, n)
		if statsTree != nil {
			statsTree.PutFile(relPath, hash, size, n)
		}
		offset += uint64(size)
		stats.UploadBytes += uint64(size)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("error walking output: %v", err)
	}
	if _, err := putObjsClient.CloseAndRecv(); err != nil && err != io.EOF {
		return nil, err
	}
	// Serialize datum hashtree
	b := &bytes.Buffer{}
	if err := tree.Serialize(b); err != nil {
		return nil, err
	}
	// Write datum hashtree to object storage
	w, err := d.pachClient.PutObjectAsync([]*pfs.Tag{client.NewTag(tag)})
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := w.Close(); err != nil && retErr != nil {
			retErr = err
		}
	}()
	if _, err := w.Write(b.Bytes()); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
