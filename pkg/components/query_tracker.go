package components

import (
	"context"
	"fmt"
	"strings"

	"go.ytsaurus.tech/library/go/ptr"
	"go.ytsaurus.tech/yt/go/ypath"
	"go.ytsaurus.tech/yt/go/yt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ytv1 "github.com/ytsaurus/yt-k8s-operator/api/v1"
	"github.com/ytsaurus/yt-k8s-operator/pkg/apiproxy"
	"github.com/ytsaurus/yt-k8s-operator/pkg/consts"
	"github.com/ytsaurus/yt-k8s-operator/pkg/labeller"
	"github.com/ytsaurus/yt-k8s-operator/pkg/resources"
	"github.com/ytsaurus/yt-k8s-operator/pkg/ytconfig"
)

type QueryTracker struct {
	localServerComponent
	cfgen *ytconfig.Generator

	ytsaurusClient internalYtsaurusClient
	tabletNodes    []Component
	initCondition  string
	initQTState    *InitJob
	secret         *resources.StringSecret
}

func NewQueryTracker(
	cfgen *ytconfig.Generator,
	ytsaurus *apiproxy.Ytsaurus,
	yc internalYtsaurusClient,
	tabletNodes []Component,
) *QueryTracker {
	resource := ytsaurus.GetResource()
	l := labeller.Labeller{
		ObjectMeta:     &resource.ObjectMeta,
		APIProxy:       ytsaurus.APIProxy(),
		ComponentLabel: "yt-query-tracker",
		ComponentName:  "QueryTracker",
		Annotations:    resource.Spec.ExtraPodAnnotations,
	}

	if resource.Spec.QueryTrackers.InstanceSpec.MonitoringPort == nil {
		resource.Spec.QueryTrackers.InstanceSpec.MonitoringPort = ptr.Int32(consts.QueryTrackerMonitoringPort)
	}

	srv := newServer(
		&l,
		ytsaurus,
		&resource.Spec.QueryTrackers.InstanceSpec,
		"/usr/bin/ytserver-query-tracker",
		"ytserver-query-tracker.yson",
		cfgen.GetQueryTrackerStatefulSetName(),
		cfgen.GetQueryTrackerServiceName(),
		func() ([]byte, error) { return cfgen.GetQueryTrackerConfig(resource.Spec.QueryTrackers) },
	)

	image := ytsaurus.GetResource().Spec.CoreImage
	if resource.Spec.QueryTrackers.InstanceSpec.Image != nil {
		image = *resource.Spec.QueryTrackers.InstanceSpec.Image
	}

	return &QueryTracker{
		localServerComponent: newLocalServerComponent(&l, ytsaurus, srv),
		cfgen:                cfgen,
		tabletNodes:          tabletNodes,
		initCondition:        "queryTrackerInitCompleted",
		ytsaurusClient:       yc,
		initQTState: NewInitJob(
			&l,
			ytsaurus.APIProxy(),
			ytsaurus,
			resource.Spec.ImagePullSecrets,
			"qt-state",
			consts.ClientConfigFileName,
			image,
			cfgen.GetNativeClientConfig),
		secret: resources.NewStringSecret(
			l.GetSecretName(),
			&l,
			ytsaurus.APIProxy()),
	}
}

func (qt *QueryTracker) IsUpdatable() bool {
	return true
}

func (qt *QueryTracker) Fetch(ctx context.Context) error {
	return resources.Fetch(ctx,
		qt.server,
		qt.initQTState,
		qt.secret,
	)
}

func (qt *QueryTracker) doSync(ctx context.Context, dry bool) (ComponentStatus, error) {
	var err error

	if ytv1.IsReadyToUpdateClusterState(qt.ytsaurus.GetClusterState()) && qt.server.needUpdate() {
		return SimpleStatus(SyncStatusNeedLocalUpdate), err
	}

	if qt.ytsaurus.GetClusterState() == ytv1.ClusterStateUpdating {
		if IsUpdatingComponent(qt.ytsaurus, qt) {
			if qt.ytsaurus.GetUpdateState() == ytv1.UpdateStateWaitingForPodsRemoval && IsUpdatingComponent(qt.ytsaurus, qt) {
				if !dry {
					err = removePods(ctx, qt.server, &qt.localComponent)
				}
				return WaitingStatus(SyncStatusUpdating, "pods removal"), err
			}

			if status, err := qt.updateQTState(ctx, dry); status != nil {
				return *status, err
			}
			if qt.ytsaurus.GetUpdateState() != ytv1.UpdateStateWaitingForPodsCreation &&
				qt.ytsaurus.GetUpdateState() != ytv1.UpdateStateWaitingForQTStateUpdate {
				return NewComponentStatus(SyncStatusReady, "Nothing to do now"), err
			}
		} else {
			return NewComponentStatus(SyncStatusReady, "Not updating component"), err
		}
	}

	if qt.secret.NeedSync(consts.TokenSecretKey, "") {
		if !dry {
			secretSpec := qt.secret.Build()
			secretSpec.StringData = map[string]string{
				consts.TokenSecretKey: ytconfig.RandString(30),
			}
			err = qt.secret.Sync(ctx)
		}
		return WaitingStatus(SyncStatusPending, qt.secret.Name()), err
	}

	if qt.NeedSync() {
		if !dry {
			err = qt.server.Sync(ctx)
		}

		return WaitingStatus(SyncStatusPending, "components"), err
	}

	if !qt.server.arePodsReady(ctx) {
		return WaitingStatus(SyncStatusBlocked, "pods"), err
	}

	// Wait for tablet nodes to proceed with query tracker state init.
	if qt.tabletNodes == nil || len(qt.tabletNodes) == 0 {
		return WaitingStatus(SyncStatusBlocked, "tablet nodes"), fmt.Errorf("cannot initialize query tracker without tablet nodes")
	}

	for _, tnd := range qt.tabletNodes {
		if !IsRunningStatus(tnd.Status(ctx).SyncStatus) {
			return WaitingStatus(SyncStatusBlocked, "tablet nodes"), err
		}
	}

	var ytClient yt.Client
	if qt.ytsaurus.GetClusterState() != ytv1.ClusterStateUpdating {
		if qt.ytsaurusClient.Status(ctx).SyncStatus != SyncStatusReady {
			return WaitingStatus(SyncStatusBlocked, qt.ytsaurusClient.GetName()), err
		}

		if !dry {
			ytClient = qt.ytsaurusClient.GetYtClient()

			err = qt.createUser(ctx, ytClient)
			if err != nil {
				return WaitingStatus(SyncStatusPending, "create qt user"), err
			}
		}
	}

	if !dry {
		qt.prepareInitQueryTrackerState()
	}
	status, err := qt.initQTState.Sync(ctx, dry)
	if err != nil || status.SyncStatus != SyncStatusReady {
		return status, err
	}

	if qt.ytsaurus.GetClusterState() != ytv1.ClusterStateUpdating {
		if !dry {
			err = qt.init(ctx, ytClient)
			if err != nil {
				return WaitingStatus(SyncStatusPending, fmt.Sprintf("%s initialization", qt.GetName())), err
			}

			qt.ytsaurus.SetStatusCondition(metav1.Condition{
				Type:    qt.initCondition,
				Status:  metav1.ConditionTrue,
				Reason:  "InitQueryTrackerCompleted",
				Message: "Init query tracker successfully completed",
			})
		}
	}

	if qt.ytsaurus.IsStatusConditionTrue(qt.initCondition) {
		return SimpleStatus(SyncStatusReady), err
	}
	return WaitingStatus(SyncStatusPending, fmt.Sprintf("setting %s condition", qt.initCondition)), err
}

func (qt *QueryTracker) createUser(ctx context.Context, ytClient yt.Client) (err error) {
	logger := log.FromContext(ctx)

	token, _ := qt.secret.GetValue(consts.TokenSecretKey)
	err = CreateUser(ctx, ytClient, "query_tracker", token, true)
	if err != nil {
		logger.Error(err, "Creating user 'query_tracker' failed")
		return
	}
	return
}

func (qt *QueryTracker) init(ctx context.Context, ytClient yt.Client) (err error) {
	logger := log.FromContext(ctx)

	_, err = ytClient.CreateNode(
		ctx,
		ypath.Path("//sys/query_tracker/config"),
		yt.NodeDocument,
		&yt.CreateNodeOptions{
			Attributes: map[string]interface{}{
				"value": map[string]interface{}{
					"query_tracker": map[string]interface{}{
						"ql_engine": map[string]interface{}{
							"default_cluster": qt.labeller.GetClusterName(),
						},
						"chyt_engine": map[string]interface{}{
							"default_cluster": qt.labeller.GetClusterName(),
						},
						"spyt_engine": map[string]interface{}{
							"default_cluster": qt.labeller.GetClusterName(),
						},
					},
				},
			},
			Recursive:      true,
			IgnoreExisting: true,
		},
	)
	if err != nil {
		logger.Error(err, "Creating document '//sys/query_tracker/config' failed")
		return
	}

	err = ytClient.SetNode(
		ctx,
		ypath.Path("//sys/@cluster_connection/query_tracker"),
		map[string]interface{}{
			"stages": map[string]interface{}{
				"production": map[string]interface{}{
					"root": "//sys/query_tracker",
					"user": "query_tracker",
				},
			},
		},
		nil,
	)
	if err != nil {
		logger.Error(err, "Setting '//sys/@cluster_connection/query_tracker' failed")
		return
	}

	clusterConnectionAttr := make(map[string]interface{})
	err = ytClient.GetNode(ctx, ypath.Path("//sys/@cluster_connection"), &clusterConnectionAttr, nil)
	if err != nil {
		logger.Error(err, "Getting '//sys/@cluster_connection' failed")
		return
	}

	err = ytClient.SetNode(
		ctx,
		ypath.Path(fmt.Sprintf("//sys/clusters/%s", qt.labeller.GetClusterName())),
		clusterConnectionAttr,
		nil,
	)
	if err != nil {
		logger.Error(err, fmt.Sprintf("Setting '//sys/clusters/%s' failed", qt.labeller.GetClusterName()))
		return
	}

	_, err = ytClient.CreateObject(
		ctx,
		yt.NodeAccessControlObjectNamespace,
		&yt.CreateObjectOptions{
			Attributes: map[string]interface{}{
				"name": "queries",
			},
			IgnoreExisting: true,
		},
	)
	if err != nil {
		logger.Error(err, "Creating access control object namespace 'queries' failed")
		return
	}

	_, err = ytClient.CreateObject(
		ctx,
		yt.NodeAccessControlObject,
		&yt.CreateObjectOptions{
			Attributes: map[string]interface{}{
				"name":      "nobody",
				"namespace": "queries",
			},
			IgnoreExisting: true,
		},
	)
	if err != nil {
		logger.Error(err, "Creating access control object 'nobody' in namespace 'queries' failed")
		return
	}

	_, err = ytClient.CreateObject(
		ctx,
		yt.NodeAccessControlObject,
		&yt.CreateObjectOptions{
			Attributes: map[string]interface{}{
				"name":      "everyone",
				"namespace": "queries",
				"principal_acl": []interface{}{map[string]interface{}{
					"action":      "allow",
					"subjects":    []string{"everyone"},
					"permissions": []string{"use"},
				}},
			},
			IgnoreExisting: true,
		},
	)
	if err != nil {
		logger.Error(err, "Creating access control object 'everyone' in namespace 'queries' failed")
		return
	}
	return
}

func (qt *QueryTracker) Status(ctx context.Context) ComponentStatus {
	status, err := qt.doSync(ctx, true)
	if err != nil {
		panic(err)
	}

	return status
}

func (qt *QueryTracker) Sync(ctx context.Context) error {
	_, err := qt.doSync(ctx, false)
	return err
}

func (qt *QueryTracker) prepareInitQueryTrackerState() {
	path := "/usr/bin/init_query_tracker_state"

	script := []string{
		initJobWithNativeDriverPrologue(),
		fmt.Sprintf("if [[ -f \"%s\" ]]; then %s --force --latest --proxy %s; fi",
			path, path, qt.cfgen.GetHTTPProxiesServiceAddress(consts.DefaultHTTPProxyRole)),
	}

	qt.initQTState.SetInitScript(strings.Join(script, "\n"))
	job := qt.initQTState.Build()
	container := &job.Spec.Template.Spec.Containers[0]
	container.EnvFrom = []corev1.EnvFromSource{qt.secret.GetEnvSource()}
}

func (qt *QueryTracker) updateQTState(ctx context.Context, dry bool) (*ComponentStatus, error) {
	var err error
	switch qt.ytsaurus.GetUpdateState() {
	case ytv1.UpdateStateWaitingForQTStateUpdatingPrepare:
		if !qt.initQTState.isRestartPrepared() {
			return ptr.T(SimpleStatus(SyncStatusUpdating)), qt.initQTState.prepareRestart(ctx, dry)
		}
		if !dry {
			qt.setConditionQTStatePreparedForUpdating(ctx)
		}
		return ptr.T(SimpleStatus(SyncStatusUpdating)), err
	case ytv1.UpdateStateWaitingForQTStateUpdate:
		if !qt.initQTState.isRestartCompleted() {
			return nil, nil
		}
		if !dry {
			qt.setConditionQTStateUpdated(ctx)
		}
		return ptr.T(SimpleStatus(SyncStatusUpdating)), err
	default:
		return nil, nil
	}
}

func (qt *QueryTracker) setConditionQTStatePreparedForUpdating(ctx context.Context) {
	qt.ytsaurus.SetUpdateStatusCondition(ctx, metav1.Condition{
		Type:    consts.ConditionQTStatePreparedForUpdating,
		Status:  metav1.ConditionTrue,
		Reason:  "QTStatePreparedForUpdating",
		Message: fmt.Sprintf("Query Tracker state prepared for updating"),
	})
}

func (qt *QueryTracker) setConditionQTStateUpdated(ctx context.Context) {
	qt.ytsaurus.SetUpdateStatusCondition(ctx, metav1.Condition{
		Type:    consts.ConditionQTStateUpdated,
		Status:  metav1.ConditionTrue,
		Reason:  "QTStateUpdated",
		Message: fmt.Sprintf("Query tracker state updated"),
	})
}
