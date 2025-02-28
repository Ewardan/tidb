// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package pdutil

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-semver/semver"
	"github.com/docker/go-units"
	"github.com/google/uuid"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/httputil"
	"github.com/pingcap/tidb/br/pkg/lightning/common"
	"github.com/pingcap/tidb/pkg/store/pdtypes"
	"github.com/pingcap/tidb/pkg/util/codec"
	pd "github.com/tikv/pd/client"
	pdhttp "github.com/tikv/pd/client/http"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

const (
	maxMsgSize   = int(128 * units.MiB) // pd.ScanRegion may return a large response
	pauseTimeout = 5 * time.Minute
	// pd request retry time when connection fail
	pdRequestRetryTime = 120
	// set max-pending-peer-count to a large value to avoid scatter region failed.
	maxPendingPeerUnlimited uint64 = math.MaxInt32
)

// pauseConfigGenerator generate a config value according to store count and current value.
type pauseConfigGenerator func(int, interface{}) interface{}

// zeroPauseConfig sets the config to 0.
func zeroPauseConfig(int, interface{}) interface{} {
	return 0
}

// pauseConfigMulStores multiplies the existing value by
// number of stores. The value is limited to 40, as larger value
// may make the cluster unstable.
func pauseConfigMulStores(stores int, raw interface{}) interface{} {
	rawCfg := raw.(float64)
	return math.Min(40, rawCfg*float64(stores))
}

// pauseConfigFalse sets the config to "false".
func pauseConfigFalse(int, interface{}) interface{} {
	return "false"
}

// constConfigGeneratorBuilder build a pauseConfigGenerator based on a given const value.
func constConfigGeneratorBuilder(val interface{}) pauseConfigGenerator {
	return func(int, interface{}) interface{} {
		return val
	}
}

// ClusterConfig represents a set of scheduler whose config have been modified
// along with their original config.
type ClusterConfig struct {
	// Enable PD schedulers before restore
	Schedulers []string `json:"schedulers"`
	// Original scheudle configuration
	ScheduleCfg map[string]interface{} `json:"schedule_cfg"`
}

type pauseSchedulerBody struct {
	Delay int64 `json:"delay"`
}

var (
	// in v4.0.8 version we can use pause configs
	// see https://github.com/tikv/pd/pull/3088
	pauseConfigVersion = semver.Version{Major: 4, Minor: 0, Patch: 8}

	// After v6.1.0 version, we can pause schedulers by key range with TTL.
	minVersionForRegionLabelTTL = semver.Version{Major: 6, Minor: 1, Patch: 0}

	// Schedulers represent region/leader schedulers which can impact on performance.
	Schedulers = map[string]struct{}{
		"balance-leader-scheduler":     {},
		"balance-hot-region-scheduler": {},
		"balance-region-scheduler":     {},

		"shuffle-leader-scheduler":     {},
		"shuffle-region-scheduler":     {},
		"shuffle-hot-region-scheduler": {},
	}
	expectPDCfgGenerators = map[string]pauseConfigGenerator{
		"merge-schedule-limit": zeroPauseConfig,
		// TODO "leader-schedule-limit" and "region-schedule-limit" don't support ttl for now,
		// but we still need set these config for compatible with old version.
		// we need wait for https://github.com/tikv/pd/pull/3131 merged.
		// see details https://github.com/pingcap/br/pull/592#discussion_r522684325
		"leader-schedule-limit":       pauseConfigMulStores,
		"region-schedule-limit":       pauseConfigMulStores,
		"max-snapshot-count":          pauseConfigMulStores,
		"enable-location-replacement": pauseConfigFalse,
		"max-pending-peer-count":      constConfigGeneratorBuilder(maxPendingPeerUnlimited),
	}

	// defaultPDCfg find by https://github.com/tikv/pd/blob/master/conf/config.toml.
	// only use for debug command.
	defaultPDCfg = map[string]interface{}{
		"merge-schedule-limit":        8,
		"leader-schedule-limit":       4,
		"region-schedule-limit":       2048,
		"enable-location-replacement": "true",
	}
)

// pdHTTPRequest defines the interface to send a request to pd and return the result in bytes.
type pdHTTPRequest func(ctx context.Context, addr string, prefix string,
	cli *http.Client, method string, body []byte) ([]byte, error)

// pdRequest is a func to send an HTTP to pd and return the result bytes.
func pdRequest(
	ctx context.Context,
	addr string, prefix string,
	cli *http.Client, method string, body []byte) ([]byte, error) {
	_, respBody, err := pdRequestWithCode(ctx, addr, prefix, cli, method, body)
	return respBody, err
}

func pdRequestWithCode(
	ctx context.Context,
	addr string, prefix string,
	cli *http.Client, method string, body []byte) (int, []byte, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return 0, nil, errors.Trace(err)
	}
	reqURL := fmt.Sprintf("%s%s", u, prefix)
	var (
		req  *http.Request
		resp *http.Response
	)
	if body == nil {
		body = []byte("")
	}
	count := 0
	// the total retry duration: 120*1 = 2min
	for {
		req, err = http.NewRequestWithContext(ctx, method, reqURL, bytes.NewBuffer(body))
		if err != nil {
			return 0, nil, errors.Trace(err)
		}
		resp, err = cli.Do(req) //nolint:bodyclose
		count++
		failpoint.Inject("InjectClosed", func(v failpoint.Value) {
			if failType, ok := v.(int); ok && count <= pdRequestRetryTime-1 {
				resp = nil
				switch failType {
				case 0:
					err = &net.OpError{
						Op:  "read",
						Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
					}
				default:
					err = &url.Error{
						Op:  "read",
						Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
					}
				}
			}
		})
		if count > pdRequestRetryTime || (resp != nil && resp.StatusCode < 500) ||
			(err != nil && !common.IsRetryableError(err)) {
			break
		}
		log.Warn("request failed, will retry later",
			zap.String("url", reqURL), zap.Int("retry-count", count), zap.Error(err))
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(pdRequestRetryInterval())
	}
	if err != nil {
		return 0, nil, errors.Trace(err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		res, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, nil, errors.Annotatef(berrors.ErrPDInvalidResponse,
			"[%d] %s %s", resp.StatusCode, res, reqURL)
	}

	r, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, errors.Trace(err)
	}
	return resp.StatusCode, r, nil
}

func pdRequestRetryInterval() time.Duration {
	failpoint.Inject("FastRetry", func(v failpoint.Value) {
		if v.(bool) {
			failpoint.Return(0)
		}
	})
	return time.Second
}

// DefaultExpectPDCfgGenerators returns default pd config generators
func DefaultExpectPDCfgGenerators() map[string]pauseConfigGenerator {
	clone := make(map[string]pauseConfigGenerator, len(expectPDCfgGenerators))
	for k := range expectPDCfgGenerators {
		clone[k] = expectPDCfgGenerators[k]
	}
	return clone
}

// PdController manage get/update config from pd.
type PdController struct {
	addrs     []string
	cli       *http.Client // TODO: replace it with pd HTTP client
	pdClient  pd.Client
	pdHTTPCli pdhttp.Client
	version   *semver.Version

	// control the pause schedulers goroutine
	schedulerPauseCh chan struct{}
}

// NewPdController creates a new PdController.
func NewPdController(
	ctx context.Context,
	pdAddrs string,
	tlsConf *tls.Config,
	securityOption pd.SecurityOption,
) (*PdController, error) {
	cli := httputil.NewClient(tlsConf)

	addrs := strings.Split(pdAddrs, ",")
	processedAddrs := make([]string, 0, len(addrs))
	var failure error
	var versionBytes []byte
	for _, addr := range addrs {
		if !strings.HasPrefix(addr, "http") {
			if tlsConf != nil {
				addr = "https://" + addr
			} else {
				addr = "http://" + addr
			}
		}
		processedAddrs = append(processedAddrs, addr)
		versionBytes, failure = pdRequest(ctx, addr, pdhttp.ClusterVersion, cli, http.MethodGet, nil)
		if failure == nil {
			break
		}
	}
	if failure != nil {
		return nil, errors.Annotatef(berrors.ErrPDUpdateFailed,
			"pd address (%s) not available, error is %s, please check network", pdAddrs, failure)
	}

	version := parseVersion(versionBytes)
	maxCallMsgSize := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxMsgSize)),
		grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(maxMsgSize)),
	}
	pdClient, err := pd.NewClientWithContext(
		ctx, addrs, securityOption,
		pd.WithGRPCDialOptions(maxCallMsgSize...),
		// If the time too short, we may scatter a region many times, because
		// the interface `ScatterRegions` may time out.
		pd.WithCustomTimeoutOption(60*time.Second),
		pd.WithMaxErrorRetry(3),
	)
	if err != nil {
		log.Error("fail to create pd client", zap.Error(err))
		return nil, errors.Trace(err)
	}

	pdHTTPCliConfig := make([]pdhttp.ClientOption, 0, 1)
	if tlsConf != nil {
		pdHTTPCliConfig = append(pdHTTPCliConfig, pdhttp.WithTLSConfig(tlsConf))
	}
	return &PdController{
		addrs:     processedAddrs,
		cli:       cli,
		pdClient:  pdClient,
		pdHTTPCli: pdhttp.NewClient("br/lightning PD controller", addrs, pdHTTPCliConfig...),
		version:   version,
		// We should make a buffered channel here otherwise when context canceled,
		// gracefully shutdown will stick at resuming schedulers.
		schedulerPauseCh: make(chan struct{}, 1),
	}, nil
}

func parseVersion(versionBytes []byte) *semver.Version {
	// we need trim space or semver will parse failed
	v := strings.TrimSpace(string(versionBytes))
	v = strings.Trim(v, "\"")
	v = strings.TrimPrefix(v, "v")
	version, err := semver.NewVersion(v)
	if err != nil {
		log.Warn("fail back to v0.0.0 version",
			zap.ByteString("version", versionBytes), zap.Error(err))
		version = &semver.Version{Major: 0, Minor: 0, Patch: 0}
	}
	failpoint.Inject("PDEnabledPauseConfig", func(val failpoint.Value) {
		if val.(bool) {
			// test pause config is enable
			version = &semver.Version{Major: 5, Minor: 0, Patch: 0}
		}
	})
	return version
}

// TODO: always read latest PD nodes from PD client
func (p *PdController) getAllPDAddrs() []string {
	ret := make([]string, 0, len(p.addrs)+1)
	if p.pdClient != nil {
		ret = append(ret, p.pdClient.GetLeaderAddr())
	}
	ret = append(ret, p.addrs...)
	return ret
}

func (p *PdController) isPauseConfigEnabled() bool {
	return p.version.Compare(pauseConfigVersion) >= 0
}

// SetHTTP set pd addrs and cli for test.
func (p *PdController) SetHTTP(addrs []string, cli *http.Client) {
	p.addrs = addrs
	p.cli = cli
}

// SetPDClient set pd addrs and cli for test.
func (p *PdController) SetPDClient(pdClient pd.Client) {
	p.pdClient = pdClient
}

// GetPDClient set pd addrs and cli for test.
func (p *PdController) GetPDClient() pd.Client {
	return p.pdClient
}

// GetPDHTTPClient returns the pd http client.
func (p *PdController) GetPDHTTPClient() pdhttp.Client {
	return p.pdHTTPCli
}

// GetClusterVersion returns the current cluster version.
func (p *PdController) GetClusterVersion(ctx context.Context) (string, error) {
	return p.getClusterVersionWith(ctx, pdRequest)
}

func (p *PdController) getClusterVersionWith(ctx context.Context, get pdHTTPRequest) (string, error) {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := get(ctx, addr, pdhttp.ClusterVersion, p.cli, http.MethodGet, nil)
		if e != nil {
			err = e
			continue
		}
		return string(v), nil
	}

	return "", errors.Trace(err)
}

// GetRegionCount returns the region count in the specified range.
func (p *PdController) GetRegionCount(ctx context.Context, startKey, endKey []byte) (int, error) {
	return p.getRegionCountWith(ctx, pdRequest, startKey, endKey)
}

func (p *PdController) getRegionCountWith(
	ctx context.Context, get pdHTTPRequest, startKey, endKey []byte,
) (int, error) {
	// TiKV reports region start/end keys to PD in memcomparable-format.
	var start, end []byte
	start = codec.EncodeBytes(nil, startKey)
	if len(endKey) != 0 { // Empty end key means the max.
		end = codec.EncodeBytes(nil, endKey)
	}
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := get(ctx, addr,
			pdhttp.RegionStatsByKeyRange(pdhttp.NewKeyRange(start, end), false),
			p.cli, http.MethodGet, nil)
		if e != nil {
			err = e
			continue
		}
		regionsMap := make(map[string]interface{})
		err = json.Unmarshal(v, &regionsMap)
		if err != nil {
			return 0, errors.Trace(err)
		}
		return int(regionsMap["count"].(float64)), nil
	}
	return 0, errors.Trace(err)
}

// GetStoreInfo returns the info of store with the specified id.
func (p *PdController) GetStoreInfo(ctx context.Context, storeID uint64) (*pdtypes.StoreInfo, error) {
	return p.getStoreInfoWith(ctx, pdRequest, storeID)
}

func (p *PdController) getStoreInfoWith(
	ctx context.Context, get pdHTTPRequest, storeID uint64) (*pdtypes.StoreInfo, error) {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := get(ctx, addr, pdhttp.StoreByID(storeID), p.cli, http.MethodGet, nil)
		if e != nil {
			err = e
			continue
		}
		store := pdtypes.StoreInfo{}
		err = json.Unmarshal(v, &store)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return &store, nil
	}
	return nil, errors.Trace(err)
}

func (p *PdController) doPauseSchedulers(ctx context.Context,
	schedulers []string, post pdHTTPRequest) ([]string, error) {
	// pause this scheduler with 300 seconds
	body, err := json.Marshal(pauseSchedulerBody{Delay: int64(pauseTimeout.Seconds())})
	if err != nil {
		return nil, errors.Trace(err)
	}
	// PauseSchedulers remove pd scheduler temporarily.
	removedSchedulers := make([]string, 0, len(schedulers))
	for _, scheduler := range schedulers {
		for _, addr := range p.getAllPDAddrs() {
			_, err = post(ctx, addr, pdhttp.SchedulerByName(scheduler), p.cli, http.MethodPost, body)
			if err == nil {
				removedSchedulers = append(removedSchedulers, scheduler)
				break
			}
		}
		if err != nil {
			return removedSchedulers, errors.Trace(err)
		}
	}
	return removedSchedulers, nil
}

func (p *PdController) pauseSchedulersAndConfigWith(
	ctx context.Context, schedulers []string,
	schedulerCfg map[string]interface{}, post pdHTTPRequest,
) ([]string, error) {
	// first pause this scheduler, if the first time failed. we should return the error
	// so put first time out of for loop. and in for loop we could ignore other failed pause.
	removedSchedulers, err := p.doPauseSchedulers(ctx, schedulers, post)
	if err != nil {
		log.Error("failed to pause scheduler at beginning",
			zap.Strings("name", schedulers), zap.Error(err))
		return nil, errors.Trace(err)
	}
	log.Info("pause scheduler successful at beginning", zap.Strings("name", schedulers))
	if schedulerCfg != nil {
		err = p.doPauseConfigs(ctx, schedulerCfg, post)
		if err != nil {
			log.Error("failed to pause config at beginning",
				zap.Any("cfg", schedulerCfg), zap.Error(err))
			return nil, errors.Trace(err)
		}
		log.Info("pause configs successful at beginning", zap.Any("cfg", schedulerCfg))
	}

	go func() {
		tick := time.NewTicker(pauseTimeout / 3)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_, err := p.doPauseSchedulers(ctx, schedulers, post)
				if err != nil {
					log.Warn("pause scheduler failed, ignore it and wait next time pause", zap.Error(err))
				}
				if schedulerCfg != nil {
					err = p.doPauseConfigs(ctx, schedulerCfg, post)
					if err != nil {
						log.Warn("pause configs failed, ignore it and wait next time pause", zap.Error(err))
					}
				}
				log.Info("pause scheduler(configs)", zap.Strings("name", removedSchedulers),
					zap.Any("cfg", schedulerCfg))
			case <-p.schedulerPauseCh:
				log.Info("exit pause scheduler and configs successful")
				return
			}
		}
	}()
	return removedSchedulers, nil
}

// ResumeSchedulers resume pd scheduler.
func (p *PdController) ResumeSchedulers(ctx context.Context, schedulers []string) error {
	return p.resumeSchedulerWith(ctx, schedulers, pdRequest)
}

func (p *PdController) resumeSchedulerWith(ctx context.Context, schedulers []string, post pdHTTPRequest) (err error) {
	log.Info("resume scheduler", zap.Strings("schedulers", schedulers))
	p.schedulerPauseCh <- struct{}{}

	// 0 means stop pause.
	body, err := json.Marshal(pauseSchedulerBody{Delay: 0})
	if err != nil {
		return errors.Trace(err)
	}
	for _, scheduler := range schedulers {
		for _, addr := range p.getAllPDAddrs() {
			_, err = post(ctx, addr, pdhttp.SchedulerByName(scheduler), p.cli, http.MethodPost, body)
			if err == nil {
				break
			}
		}
		if err != nil {
			log.Error("failed to resume scheduler after retry, you may reset this scheduler manually"+
				"or just wait this scheduler pause timeout", zap.String("scheduler", scheduler))
		} else {
			log.Info("resume scheduler successful", zap.String("scheduler", scheduler))
		}
	}
	// no need to return error, because the pause will timeout.
	return nil
}

// ListSchedulers list all pd scheduler.
func (p *PdController) ListSchedulers(ctx context.Context) ([]string, error) {
	return p.listSchedulersWith(ctx, pdRequest)
}

func (p *PdController) listSchedulersWith(ctx context.Context, get pdHTTPRequest) ([]string, error) {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := get(ctx, addr, pdhttp.Schedulers, p.cli, http.MethodGet, nil)
		if e != nil {
			err = e
			continue
		}
		d := make([]string, 0)
		err = json.Unmarshal(v, &d)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return d, nil
	}
	return nil, errors.Trace(err)
}

// GetPDScheduleConfig returns PD schedule config value associated with the key.
// It returns nil if there is no such config item.
func (p *PdController) GetPDScheduleConfig(
	ctx context.Context,
) (map[string]interface{}, error) {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := pdRequest(
			ctx, addr, pdhttp.ScheduleConfig, p.cli, http.MethodGet, nil)
		if e != nil {
			err = e
			continue
		}
		cfg := make(map[string]interface{})
		err = json.Unmarshal(v, &cfg)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return cfg, nil
	}
	return nil, errors.Trace(err)
}

// UpdatePDScheduleConfig updates PD schedule config value associated with the key.
func (p *PdController) UpdatePDScheduleConfig(ctx context.Context) error {
	log.Info("update pd with default config", zap.Any("cfg", defaultPDCfg))
	return p.doUpdatePDScheduleConfig(ctx, defaultPDCfg, pdRequest)
}

func (p *PdController) doUpdatePDScheduleConfig(
	ctx context.Context, cfg map[string]interface{}, post pdHTTPRequest, prefixs ...string,
) error {
	prefix := pdhttp.Config
	if len(prefixs) != 0 {
		prefix = prefixs[0]
	}
	newCfg := make(map[string]interface{})
	for k, v := range cfg {
		// if we want use ttl, we need use config prefix first.
		// which means cfg should transfer from "max-merge-region-keys" to "schedule.max-merge-region-keys".
		sc := fmt.Sprintf("schedule.%s", k)
		newCfg[sc] = v
	}

	for _, addr := range p.getAllPDAddrs() {
		reqData, err := json.Marshal(newCfg)
		if err != nil {
			return errors.Trace(err)
		}
		_, e := post(ctx, addr, prefix,
			p.cli, http.MethodPost, reqData)
		if e == nil {
			return nil
		}
		log.Warn("failed to update PD config, will try next", zap.Error(e), zap.String("pd", addr))
	}
	return errors.Annotate(berrors.ErrPDUpdateFailed, "failed to update PD schedule config")
}

func (p *PdController) doPauseConfigs(ctx context.Context, cfg map[string]interface{}, post pdHTTPRequest) error {
	// pause this scheduler with 300 seconds
	return p.doUpdatePDScheduleConfig(ctx, cfg, post, pdhttp.ConfigWithTTLSeconds(pauseTimeout.Seconds()))
}

func restoreSchedulers(ctx context.Context, pd *PdController, clusterCfg ClusterConfig,
	configsNeedRestore map[string]pauseConfigGenerator) error {
	if err := pd.ResumeSchedulers(ctx, clusterCfg.Schedulers); err != nil {
		return errors.Annotate(err, "fail to add PD schedulers")
	}
	log.Info("restoring config", zap.Any("config", clusterCfg.ScheduleCfg))
	mergeCfg := make(map[string]interface{})
	for cfgKey := range configsNeedRestore {
		value := clusterCfg.ScheduleCfg[cfgKey]
		if value == nil {
			// Ignore non-exist config.
			continue
		}
		mergeCfg[cfgKey] = value
	}

	prefix := make([]string, 0, 1)
	if pd.isPauseConfigEnabled() {
		// set config's ttl to zero, make temporary config invalid immediately.
		prefix = append(prefix, pdhttp.ConfigWithTTLSeconds(0))
	}
	// reset config with previous value.
	if err := pd.doUpdatePDScheduleConfig(ctx, mergeCfg, pdRequest, prefix...); err != nil {
		return errors.Annotate(err, "fail to update PD merge config")
	}
	return nil
}

// MakeUndoFunctionByConfig return an UndoFunc based on specified ClusterConfig
func (p *PdController) MakeUndoFunctionByConfig(config ClusterConfig) UndoFunc {
	return p.GenRestoreSchedulerFunc(config, expectPDCfgGenerators)
}

// GenRestoreSchedulerFunc gen restore func
func (p *PdController) GenRestoreSchedulerFunc(config ClusterConfig,
	configsNeedRestore map[string]pauseConfigGenerator) UndoFunc {
	// todo: we only need config names, not a map[string]pauseConfigGenerator
	restore := func(ctx context.Context) error {
		return restoreSchedulers(ctx, p, config, configsNeedRestore)
	}
	return restore
}

// RemoveSchedulers removes the schedulers that may slow down BR speed.
func (p *PdController) RemoveSchedulers(ctx context.Context) (undo UndoFunc, err error) {
	undo = Nop

	origin, _, err1 := p.RemoveSchedulersWithOrigin(ctx)
	if err1 != nil {
		err = err1
		return
	}

	undo = p.MakeUndoFunctionByConfig(ClusterConfig{Schedulers: origin.Schedulers, ScheduleCfg: origin.ScheduleCfg})
	return undo, errors.Trace(err)
}

// RemoveSchedulersWithConfig removes the schedulers that may slow down BR speed.
func (p *PdController) RemoveSchedulersWithConfig(
	ctx context.Context,
) (undo UndoFunc, config *ClusterConfig, err error) {
	undo = Nop

	origin, _, err1 := p.RemoveSchedulersWithOrigin(ctx)
	if err1 != nil {
		err = err1
		return
	}

	undo = p.MakeUndoFunctionByConfig(ClusterConfig{Schedulers: origin.Schedulers, ScheduleCfg: origin.ScheduleCfg})
	return undo, &origin, errors.Trace(err)
}

// RemoveAllPDSchedulers pause pd scheduler during the snapshot backup and restore
func (p *PdController) RemoveAllPDSchedulers(ctx context.Context) (undo UndoFunc, err error) {
	undo = Nop

	// during the backup, we shall stop all scheduler so that restore easy to implement
	// during phase-2, pd is fresh and in recovering-mode(recovering-mark=true), there's no leader
	// so there's no leader or region schedule initially. when phase-2 start force setting leaders, schedule may begin.
	// we don't want pd do any leader or region schedule during this time, so we set those params to 0
	// before we force setting leaders
	const enableTiKVSplitRegion = "enable-tikv-split-region"
	scheduleLimitParams := []string{
		"hot-region-schedule-limit",
		"leader-schedule-limit",
		"merge-schedule-limit",
		"region-schedule-limit",
		"replica-schedule-limit",
		enableTiKVSplitRegion,
	}
	pdConfigGenerators := DefaultExpectPDCfgGenerators()
	for _, param := range scheduleLimitParams {
		if param == enableTiKVSplitRegion {
			pdConfigGenerators[param] = func(int, interface{}) interface{} { return false }
		} else {
			pdConfigGenerators[param] = func(int, interface{}) interface{} { return 0 }
		}
	}

	oldPDConfig, _, err1 := p.RemoveSchedulersWithConfigGenerator(ctx, pdConfigGenerators)
	if err1 != nil {
		err = err1
		return
	}

	undo = p.GenRestoreSchedulerFunc(oldPDConfig, pdConfigGenerators)
	return undo, errors.Trace(err)
}

// RemoveSchedulersWithOrigin pause and remove br related schedule configs and return the origin and modified configs
func (p *PdController) RemoveSchedulersWithOrigin(ctx context.Context) (origin ClusterConfig,
	modified ClusterConfig, err error) {
	return p.RemoveSchedulersWithConfigGenerator(ctx, expectPDCfgGenerators)
}

// RemoveSchedulersWithConfigGenerator pause scheduler with custom config generator
func (p *PdController) RemoveSchedulersWithConfigGenerator(ctx context.Context,
	pdConfigGenerators map[string]pauseConfigGenerator) (
	origin ClusterConfig, modified ClusterConfig, err error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("PdController.RemoveSchedulers",
			opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	originCfg := ClusterConfig{}
	removedCfg := ClusterConfig{}
	stores, err := p.pdClient.GetAllStores(ctx)
	if err != nil {
		return originCfg, removedCfg, err
	}
	scheduleCfg, err := p.GetPDScheduleConfig(ctx)
	if err != nil {
		return originCfg, removedCfg, err
	}
	disablePDCfg := make(map[string]interface{}, len(pdConfigGenerators))
	originPDCfg := make(map[string]interface{}, len(pdConfigGenerators))
	for cfgKey, cfgValFunc := range pdConfigGenerators {
		value, ok := scheduleCfg[cfgKey]
		if !ok {
			// Ignore non-exist config.
			continue
		}
		disablePDCfg[cfgKey] = cfgValFunc(len(stores), value)
		originPDCfg[cfgKey] = value
	}
	originCfg.ScheduleCfg = originPDCfg
	removedCfg.ScheduleCfg = disablePDCfg

	log.Debug("saved PD config", zap.Any("config", scheduleCfg))

	// Remove default PD scheduler that may affect restore process.
	existSchedulers, err := p.ListSchedulers(ctx)
	if err != nil {
		return originCfg, removedCfg, err
	}
	needRemoveSchedulers := make([]string, 0, len(existSchedulers))
	for _, s := range existSchedulers {
		if _, ok := Schedulers[s]; ok {
			needRemoveSchedulers = append(needRemoveSchedulers, s)
		}
	}

	removedSchedulers, err := p.doRemoveSchedulersWith(ctx, needRemoveSchedulers, disablePDCfg)
	if err != nil {
		return originCfg, removedCfg, err
	}

	originCfg.Schedulers = removedSchedulers
	removedCfg.Schedulers = removedSchedulers

	return originCfg, removedCfg, nil
}

// RemoveSchedulersWithCfg removes pd schedulers and configs with specified ClusterConfig
func (p *PdController) RemoveSchedulersWithCfg(ctx context.Context, removeCfg ClusterConfig) error {
	_, err := p.doRemoveSchedulersWith(ctx, removeCfg.Schedulers, removeCfg.ScheduleCfg)
	return err
}

func (p *PdController) doRemoveSchedulersWith(
	ctx context.Context,
	needRemoveSchedulers []string,
	disablePDCfg map[string]interface{},
) ([]string, error) {
	var removedSchedulers []string
	var err error
	if p.isPauseConfigEnabled() {
		// after 4.0.8 we can set these config with TTL
		removedSchedulers, err = p.pauseSchedulersAndConfigWith(ctx, needRemoveSchedulers, disablePDCfg, pdRequest)
	} else {
		// adapt to earlier version (before 4.0.8) of pd cluster
		// which doesn't have temporary config setting.
		err = p.doUpdatePDScheduleConfig(ctx, disablePDCfg, pdRequest)
		if err != nil {
			return nil, err
		}
		removedSchedulers, err = p.pauseSchedulersAndConfigWith(ctx, needRemoveSchedulers, nil, pdRequest)
	}
	return removedSchedulers, err
}

// GetMinResolvedTS get min-resolved-ts from pd
func (p *PdController) GetMinResolvedTS(ctx context.Context) (uint64, error) {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		v, e := pdRequest(ctx, addr, pdhttp.MinResolvedTSPrefix, p.cli, http.MethodGet, nil)
		if e != nil {
			log.Warn("failed to get min resolved ts", zap.String("addr", addr), zap.Error(e))
			err = e
			continue
		}
		log.Info("min resolved ts", zap.String("resp", string(v)))
		d := struct {
			IsRealTime    bool   `json:"is_real_time,omitempty"`
			MinResolvedTS uint64 `json:"min_resolved_ts"`
		}{}
		err = json.Unmarshal(v, &d)
		if err != nil {
			return 0, errors.Trace(err)
		}
		if !d.IsRealTime {
			message := "min resolved ts not enabled"
			log.Error(message, zap.String("addr", addr))
			return 0, errors.Trace(errors.New(message))
		}
		return d.MinResolvedTS, nil
	}
	return 0, errors.Trace(err)
}

// RecoverBaseAllocID recover base alloc id
func (p *PdController) RecoverBaseAllocID(ctx context.Context, id uint64) error {
	reqData, _ := json.Marshal(&struct {
		ID string `json:"id"`
	}{
		ID: fmt.Sprintf("%d", id),
	})
	var err error
	for _, addr := range p.getAllPDAddrs() {
		_, e := pdRequest(ctx, addr, pdhttp.BaseAllocID, p.cli, http.MethodPost, reqData)
		if e != nil {
			log.Warn("failed to recover base alloc id", zap.String("addr", addr), zap.Error(e))
			err = e
			continue
		}
		return nil
	}
	return errors.Trace(err)
}

// ResetTS reset current ts of pd
func (p *PdController) ResetTS(ctx context.Context, ts uint64) error {
	// reset-ts of PD will never set ts < current pd ts
	// we set force-use-larger=true to allow ts > current pd ts + 24h(on default)
	reqData, _ := json.Marshal(&struct {
		Tso            string `json:"tso"`
		ForceUseLarger bool   `json:"force-use-larger"`
	}{
		Tso:            fmt.Sprintf("%d", ts),
		ForceUseLarger: true,
	})
	var err error
	for _, addr := range p.getAllPDAddrs() {
		code, _, e := pdRequestWithCode(ctx, addr, pdhttp.ResetTS, p.cli, http.MethodPost, reqData)
		if e != nil {
			// for pd version <= 6.2, if the given ts < current ts of pd, pd returns StatusForbidden.
			// it's not an error for br
			if code == http.StatusForbidden {
				log.Info("reset-ts returns with status forbidden, ignore")
				return nil
			}
			log.Warn("failed to reset ts", zap.Uint64("ts", ts), zap.String("addr", addr), zap.Error(e))
			err = e
			continue
		}
		return nil
	}
	return errors.Trace(err)
}

// MarkRecovering mark pd into recovering
func (p *PdController) MarkRecovering(ctx context.Context) error {
	return p.operateRecoveringMark(ctx, http.MethodPost)
}

// UnmarkRecovering unmark pd recovering
func (p *PdController) UnmarkRecovering(ctx context.Context) error {
	return p.operateRecoveringMark(ctx, http.MethodDelete)
}

func (p *PdController) operateRecoveringMark(ctx context.Context, method string) error {
	var err error
	for _, addr := range p.getAllPDAddrs() {
		_, e := pdRequest(ctx, addr, pdhttp.SnapshotRecoveringMark, p.cli, method, nil)
		if e != nil {
			log.Warn("failed to operate recovering mark", zap.String("method", method),
				zap.String("addr", addr), zap.Error(e))
			err = e
			continue
		}
		return nil
	}
	return errors.Trace(err)
}

// RegionLabel is the label of a region. This struct is partially copied from
// https://github.com/tikv/pd/blob/783d060861cef37c38cbdcab9777fe95c17907fe/server/schedule/labeler/rules.go#L31.
type RegionLabel struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	TTL     string `json:"ttl,omitempty"`
	StartAt string `json:"start_at,omitempty"`
}

// LabelRule is the rule to assign labels to a region. This struct is partially copied from
// https://github.com/tikv/pd/blob/783d060861cef37c38cbdcab9777fe95c17907fe/server/schedule/labeler/rules.go#L41.
type LabelRule struct {
	ID       string        `json:"id"`
	Labels   []RegionLabel `json:"labels"`
	RuleType string        `json:"rule_type"`
	Data     interface{}   `json:"data"`
}

// KeyRangeRule contains the start key and end key of the LabelRule. This struct is partially copied from
// https://github.com/tikv/pd/blob/783d060861cef37c38cbdcab9777fe95c17907fe/server/schedule/labeler/rules.go#L62.
type KeyRangeRule struct {
	StartKeyHex string `json:"start_key"` // hex format start key, for marshal/unmarshal
	EndKeyHex   string `json:"end_key"`   // hex format end key, for marshal/unmarshal
}

// PauseSchedulersByKeyRange will pause schedulers for regions in the specific key range.
// This function will spawn a goroutine to keep pausing schedulers periodically until the context is done.
// The return done channel is used to notify the caller that the background goroutine is exited.
func PauseSchedulersByKeyRange(
	ctx context.Context,
	pdHTTPCli pdhttp.Client,
	startKey, endKey []byte,
) (done <-chan struct{}, err error) {
	done, err = pauseSchedulerByKeyRangeWithTTL(ctx, pdHTTPCli, startKey, endKey, pauseTimeout)
	// Wait for the rule to take effect because the PD operator is processed asynchronously.
	// To synchronize this, checking the operator status may not be enough. For details, see
	// https://github.com/pingcap/tidb/issues/49477.
	// Let's use two times default value of `patrol-region-interval` from PD configuration.
	<-time.After(20 * time.Millisecond)
	return
}

func pauseSchedulerByKeyRangeWithTTL(
	ctx context.Context,
	pdHTTPCli pdhttp.Client,
	startKey, endKey []byte,
	ttl time.Duration,
) (<-chan struct{}, error) {
	rule := &pdhttp.LabelRule{
		ID: uuid.New().String(),
		Labels: []pdhttp.RegionLabel{{
			Key:   "schedule",
			Value: "deny",
			TTL:   ttl.String(),
		}},
		RuleType: "key-range",
		// Data should be a list of KeyRangeRule when rule type is key-range.
		// See https://github.com/tikv/pd/blob/783d060861cef37c38cbdcab9777fe95c17907fe/server/schedule/labeler/rules.go#L169.
		Data: []KeyRangeRule{{
			StartKeyHex: hex.EncodeToString(startKey),
			EndKeyHex:   hex.EncodeToString(endKey),
		}},
	}
	done := make(chan struct{})

	if err := pdHTTPCli.SetRegionLabelRule(ctx, rule); err != nil {
		close(done)
		return nil, errors.Trace(err)
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
	loop:
		for {
			select {
			case <-ticker.C:
				if err := pdHTTPCli.SetRegionLabelRule(ctx, rule); err != nil {
					if berrors.IsContextCanceled(err) {
						break loop
					}
					log.Warn("pause scheduler by key range failed, ignore it and wait next time pause",
						zap.Error(err))
				}
			case <-ctx.Done():
				break loop
			}
		}
		// Use a new context to avoid the context is canceled by the caller.
		recoverCtx, cancel := context.WithTimeout(context.Background(), time.Second*5)
		defer cancel()
		// Set ttl to 0 to remove the rule.
		rule.Labels[0].TTL = time.Duration(0).String()
		deleteRule := &pdhttp.LabelRulePatch{DeleteRules: []string{rule.ID}}
		if err := pdHTTPCli.PatchRegionLabelRules(recoverCtx, deleteRule); err != nil {
			log.Warn("failed to delete region label rule, the rule will be removed after ttl expires",
				zap.String("rule-id", rule.ID), zap.Duration("ttl", ttl), zap.Error(err))
		}
	}()
	return done, nil
}

// CanPauseSchedulerByKeyRange returns whether the scheduler can be paused by key range.
func (p *PdController) CanPauseSchedulerByKeyRange() bool {
	// We need ttl feature to ensure scheduler can recover from pause automatically.
	return p.version.Compare(minVersionForRegionLabelTTL) >= 0
}

// Close closes the connection to pd.
func (p *PdController) Close() {
	p.pdClient.Close()
	if p.pdHTTPCli != nil {
		// nil in some unit tests
		p.pdHTTPCli.Close()
	}
	if p.schedulerPauseCh != nil {
		close(p.schedulerPauseCh)
	}
}

// FetchPDVersion get pd version
func FetchPDVersion(ctx context.Context, tls *common.TLS, pdAddr string) (*semver.Version, error) {
	// An example of PD version API.
	// curl http://pd_address/pd/api/v1/version
	// {
	//   "version": "v4.0.0-rc.2-451-g760fb650"
	// }
	var rawVersion struct {
		Version string `json:"version"`
	}
	err := tls.WithHost(pdAddr).GetJSON(ctx, pdhttp.Version, &rawVersion)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return parseVersion([]byte(rawVersion.Version)), nil
}
