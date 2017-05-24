package cluster

// Postgres ThirdPartyResource object i.e. Spilo

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sync"

	"github.com/Sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api"
	"k8s.io/client-go/pkg/api/resource"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/pkg/apis/apps/v1beta1"
	"k8s.io/client-go/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/zalando-incubator/postgres-operator/pkg/spec"
	"github.com/zalando-incubator/postgres-operator/pkg/util"
	"github.com/zalando-incubator/postgres-operator/pkg/util/config"
	"github.com/zalando-incubator/postgres-operator/pkg/util/constants"
	"github.com/zalando-incubator/postgres-operator/pkg/util/k8sutil"
	"github.com/zalando-incubator/postgres-operator/pkg/util/teams"
	"github.com/zalando-incubator/postgres-operator/pkg/util/users"
	"github.com/zalando-incubator/postgres-operator/pkg/util/volumes"
	"strings"
)

var (
	alphaNumericRegexp = regexp.MustCompile("^[a-zA-Z][a-zA-Z0-9]*$")
	userRegexp         = regexp.MustCompile(`^[a-z0-9]([-_a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-_a-z0-9]*[a-z0-9])?)*$`)
	ext2fsSuccessRegexp = regexp.MustCompile(`The filesystem on [/a-z0-9]+ is now \d+ \(\d+\w+\) blocks long.`)
)

//TODO: remove struct duplication
type Config struct {
	KubeClient          *kubernetes.Clientset //TODO: move clients to the better place?
	RestClient          *rest.RESTClient
	RestConfig          *rest.Config
	TeamsAPIClient      *teams.API
	OpConfig            config.Config
	InfrastructureRoles map[string]spec.PgUser // inherited from the controller
}

type kubeResources struct {
	Service     *v1.Service
	Endpoint    *v1.Endpoints
	Secrets     map[types.UID]*v1.Secret
	Statefulset *v1beta1.StatefulSet
	//Pods are treated separately
	//PVCs are treated separately
}

type Cluster struct {
	kubeResources
	spec.Postgresql
	Config
	logger           *logrus.Entry
	pgUsers          map[string]spec.PgUser
	systemUsers      map[string]spec.PgUser
	podSubscribers   map[spec.NamespacedName]chan spec.PodEvent
	podSubscribersMu sync.RWMutex
	pgDb             *sql.DB
	mu               sync.Mutex
	masterLess       bool
	userSyncStrategy spec.UserSyncer
	deleteOptions    *v1.DeleteOptions
	podEventsQueue   *cache.FIFO
}

type compareStatefulsetResult struct {
	match         bool
	replace       bool
	rollingUpdate bool
	reasons       []string
}

func New(cfg Config, pgSpec spec.Postgresql, logger *logrus.Entry) *Cluster {
	lg := logger.WithField("pkg", "cluster").WithField("cluster-name", pgSpec.Metadata.Name)
	kubeResources := kubeResources{Secrets: make(map[types.UID]*v1.Secret)}
	orphanDependents := true

	podEventsQueue := cache.NewFIFO(func(obj interface{}) (string, error) {
		e, ok := obj.(spec.PodEvent)
		if !ok {
			return "", fmt.Errorf("could not cast to PodEvent")
		}

		return fmt.Sprintf("%s-%s", e.PodName, e.ResourceVersion), nil
	})

	cluster := &Cluster{
		Config:           cfg,
		Postgresql:       pgSpec,
		logger:           lg,
		pgUsers:          make(map[string]spec.PgUser),
		systemUsers:      make(map[string]spec.PgUser),
		podSubscribers:   make(map[spec.NamespacedName]chan spec.PodEvent),
		kubeResources:    kubeResources,
		masterLess:       false,
		userSyncStrategy: users.DefaultUserSyncStrategy{},
		deleteOptions:    &v1.DeleteOptions{OrphanDependents: &orphanDependents},
		podEventsQueue:   podEventsQueue,
	}

	return cluster
}

func (c *Cluster) ClusterName() spec.NamespacedName {
	return util.NameFromMeta(c.Metadata)
}

func (c *Cluster) teamName() string {
	// TODO: check Teams API for the actual name (in case the user passes an integer Id).
	return c.Spec.TeamID
}

func (c *Cluster) setStatus(status spec.PostgresStatus) {
	c.Status = status
	b, err := json.Marshal(status)
	if err != nil {
		c.logger.Fatalf("could not marshal status: %v", err)
	}
	request := []byte(fmt.Sprintf(`{"status": %s}`, string(b))) //TODO: Look into/wait for k8s go client methods

	_, err = c.RestClient.Patch(api.MergePatchType).
		RequestURI(c.Metadata.GetSelfLink()).
		Body(request).
		DoRaw()

	if k8sutil.ResourceNotFound(err) {
		c.logger.Warningf("could not set status for the non-existing cluster")
		return
	}

	if err != nil {
		c.logger.Warningf("could not set status for cluster '%s': %s", c.ClusterName(), err)
	}
}

func (c *Cluster) initUsers() error {
	c.initSystemUsers()

	if err := c.initInfrastructureRoles(); err != nil {
		return fmt.Errorf("could not init infrastructure roles: %v", err)
	}

	if err := c.initRobotUsers(); err != nil {
		return fmt.Errorf("could not init robot users: %v", err)
	}

	if err := c.initHumanUsers(); err != nil {
		return fmt.Errorf("could not init human users: %v", err)
	}

	c.logger.Debugf("Initialized users: %# v", util.Pretty(c.pgUsers))

	return nil
}

func (c *Cluster) Create() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var err error

	defer func() {
		if err == nil {
			c.setStatus(spec.ClusterStatusRunning) //TODO: are you sure it's running?
		} else {
			c.setStatus(spec.ClusterStatusAddFailed)
		}
	}()

	c.setStatus(spec.ClusterStatusCreating)

	//TODO: service will create endpoint implicitly
	ep, err := c.createEndpoint()
	if err != nil {
		return fmt.Errorf("could not create endpoint: %v", err)
	}
	c.logger.Infof("endpoint '%s' has been successfully created", util.NameFromMeta(ep.ObjectMeta))

	service, err := c.createService()
	if err != nil {
		return fmt.Errorf("could not create service: %v", err)
	}
	c.logger.Infof("service '%s' has been successfully created", util.NameFromMeta(service.ObjectMeta))

	if err = c.initUsers(); err != nil {
		return err
	}
	c.logger.Infof("User secrets have been initialized")

	if err = c.applySecrets(); err != nil {
		return fmt.Errorf("could not create secrets: %v", err)
	}
	c.logger.Infof("secrets have been successfully created")

	ss, err := c.createStatefulSet()
	if err != nil {
		return fmt.Errorf("could not create statefulset: %v", err)
	}
	c.logger.Infof("statefulset '%s' has been successfully created", util.NameFromMeta(ss.ObjectMeta))

	c.logger.Info("Waiting for cluster being ready")

	if err = c.waitStatefulsetPodsReady(); err != nil {
		c.logger.Errorf("Failed to create cluster: %s", err)
		return err
	}
	c.logger.Infof("pods are ready")

	if !(c.masterLess || c.databaseAccessDisabled()) {
		if err := c.initDbConn(); err != nil {
			return fmt.Errorf("could not init db connection: %v", err)
		}
		if err = c.createUsers(); err != nil {
			return fmt.Errorf("could not create users: %v", err)
		}
		c.logger.Infof("Users have been successfully created")
	} else {
		if c.masterLess {
			c.logger.Warnln("Cluster is masterless")
		}
	}

	err = c.ListResources()
	if err != nil {
		c.logger.Errorf("could not list resources: %s", err)
	}

	return nil
}

func (c *Cluster) sameServiceWith(service *v1.Service) (match bool, reason string) {
	//TODO: improve comparison
	if !reflect.DeepEqual(c.Service.Spec.LoadBalancerSourceRanges, service.Spec.LoadBalancerSourceRanges) {
		reason = "new service's LoadBalancerSourceRange doesn't match the current one"
	} else {
		match = true
	}
	return
}

func (c *Cluster) sameVolumeWith(volume spec.Volume) (match bool, reason string) {
	if !reflect.DeepEqual(c.Spec.Volume, volume) {
		reason = "new volume's specification doesn't match the current one"
	} else {
		match = true
	}
	return
}

func (c *Cluster) compareStatefulSetWith(statefulSet *v1beta1.StatefulSet) *compareStatefulsetResult {
	reasons := make([]string, 0)
	var match, needsRollUpdate, needsReplace bool

	match = true
	//TODO: improve me
	if *c.Statefulset.Spec.Replicas != *statefulSet.Spec.Replicas {
		match = false
		reasons = append(reasons, "new statefulset's number of replicas doesn't match the current one")
	}
	if len(c.Statefulset.Spec.Template.Spec.Containers) != len(statefulSet.Spec.Template.Spec.Containers) {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container specification doesn't match the current one")
	}
	if len(c.Statefulset.Spec.Template.Spec.Containers) == 0 {

		c.logger.Warnf("statefulset '%s' has no container", util.NameFromMeta(c.Statefulset.ObjectMeta))
		return &compareStatefulsetResult{}
	}
	// In the comparisons below, the needsReplace and needsRollUpdate flags are never reset, since checks fall through
	// and the combined effect of all the changes should be applied.
	// TODO: log all reasons for changing the statefulset, not just the last one.
	// TODO: make sure this is in sync with genPodTemplate, ideally by using the same list of fields to generate
	// the template and the diff
	if c.Statefulset.Spec.Template.Spec.ServiceAccountName != statefulSet.Spec.Template.Spec.ServiceAccountName {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's serviceAccountName service asccount name doesn't match the current one")
	}
	if *c.Statefulset.Spec.Template.Spec.TerminationGracePeriodSeconds != *statefulSet.Spec.Template.Spec.TerminationGracePeriodSeconds {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's terminationGracePeriodSeconds  doesn't match the current one")
	}
	// Some generated fields like creationTimestamp make it not possible to use DeepCompare on Spec.Template.ObjectMeta
	if !reflect.DeepEqual(c.Statefulset.Spec.Template.Labels, statefulSet.Spec.Template.Labels) {
		needsReplace = true
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's metadata labels doesn't match the current one")
	}
	if !reflect.DeepEqual(c.Statefulset.Spec.Template.Annotations, statefulSet.Spec.Template.Annotations) {
		needsRollUpdate = true
		needsReplace = true
		reasons = append(reasons, "new statefulset's metadata annotations doesn't match the current one")
	}
	if len(c.Statefulset.Spec.VolumeClaimTemplates) != len(statefulSet.Spec.VolumeClaimTemplates) {
		needsReplace = true
		reasons = append(reasons, "new statefulset's volumeClaimTemplates contains different number of volumes to the old one")
	}
	for i := 0; i < len(c.Statefulset.Spec.VolumeClaimTemplates); i++ {
		name := c.Statefulset.Spec.VolumeClaimTemplates[i].Name
		// Some generated fields like creationTimestamp make it not possible to use DeepCompare on ObjectMeta
		if name != statefulSet.Spec.VolumeClaimTemplates[i].Name {
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's name for volume %d doesn't match the current one", i))
			continue
		}
		if !reflect.DeepEqual(c.Statefulset.Spec.VolumeClaimTemplates[i].Annotations, statefulSet.Spec.VolumeClaimTemplates[i].Annotations) {
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's annotations for volume %s doesn't match the current one", name))
		}
		if !reflect.DeepEqual(c.Statefulset.Spec.VolumeClaimTemplates[i].Spec, statefulSet.Spec.VolumeClaimTemplates[i].Spec) {
			name := c.Statefulset.Spec.VolumeClaimTemplates[i].Name
			needsReplace = true
			reasons = append(reasons, fmt.Sprintf("new statefulset's volumeClaimTemplates specification for volume %s doesn't match the current one", name))
		}
	}

	container1 := c.Statefulset.Spec.Template.Spec.Containers[0]
	container2 := statefulSet.Spec.Template.Spec.Containers[0]
	if container1.Image != container2.Image {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container image doesn't match the current one")
	}

	if !reflect.DeepEqual(container1.Ports, container2.Ports) {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container ports don't match the current one")
	}

	if !compareResources(&container1.Resources, &container2.Resources) {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container resources don't match the current ones")
	}
	if !reflect.DeepEqual(container1.Env, container2.Env) {
		needsRollUpdate = true
		reasons = append(reasons, "new statefulset's container environment doesn't match the current one")
	}

	if needsRollUpdate || needsReplace {
		match = false
	}
	return &compareStatefulsetResult{match: match, reasons: reasons, rollingUpdate: needsRollUpdate, replace: needsReplace}
}

func compareResources(a *v1.ResourceRequirements, b *v1.ResourceRequirements) (equal bool) {
	equal = true
	if a != nil {
		equal = compareResoucesAssumeFirstNotNil(a, b)
	}
	if equal && (b != nil) {
		equal = compareResoucesAssumeFirstNotNil(b, a)
	}
	return
}

func compareResoucesAssumeFirstNotNil(a *v1.ResourceRequirements, b *v1.ResourceRequirements) bool {
	if b == nil || (len(b.Requests) == 0) {
		return (len(a.Requests) == 0)
	}
	for k, v := range a.Requests {
		if (&v).Cmp(b.Requests[k]) != 0 {
			return false
		}
	}
	for k, v := range a.Limits {
		if (&v).Cmp(b.Limits[k]) != 0 {
			return false
		}
	}
	return true

}

func (c *Cluster) Update(newSpec *spec.Postgresql) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setStatus(spec.ClusterStatusUpdating)
	c.logger.Debugf("Cluster update from version %s to %s",
		c.Metadata.ResourceVersion, newSpec.Metadata.ResourceVersion)

	newService := c.genService(newSpec.Spec.AllowedSourceRanges)
	if match, reason := c.sameServiceWith(newService); !match {
		c.logServiceChanges(c.Service, newService, true, reason)
		if err := c.updateService(newService); err != nil {
			c.setStatus(spec.ClusterStatusUpdateFailed)
			return fmt.Errorf("could not update service: %v", err)
		}
		c.logger.Infof("service '%s' has been updated", util.NameFromMeta(c.Service.ObjectMeta))
	}

	if match, reason := c.sameVolumeWith(newSpec.Spec.Volume); !match {
		c.logVolumeChanges(c.Spec.Volume, newSpec.Spec.Volume, reason)
		if err := c.updateVolumes(newSpec.Spec.Volume); err != nil {
			return fmt.Errorf("Could not update volumes: %s", err)
		}
		c.logger.Infof("volumes have been updated successfully")
	}

	newStatefulSet, err := c.genStatefulSet(newSpec.Spec)
	if err != nil {
		return fmt.Errorf("could not generate statefulset: %v", err)
	}
	cmp := c.compareStatefulSetWith(newStatefulSet)

	if !cmp.match {
		c.logStatefulSetChanges(c.Statefulset, newStatefulSet, true, cmp.reasons)
		//TODO: mind the case of updating allowedSourceRanges
		if !cmp.replace {
			if err := c.updateStatefulSet(newStatefulSet); err != nil {
				c.setStatus(spec.ClusterStatusUpdateFailed)
				return fmt.Errorf("could not upate statefulset: %v", err)
			}
		} else {
			if err := c.replaceStatefulSet(newStatefulSet); err != nil {
				c.setStatus(spec.ClusterStatusUpdateFailed)
				return fmt.Errorf("could not replace statefulset: %v", err)
			}
		}
		//TODO: if there is a change in numberOfInstances, make sure Pods have been created/deleted
		c.logger.Infof("statefulset '%s' has been updated", util.NameFromMeta(c.Statefulset.ObjectMeta))
	}

	if c.Spec.PgVersion != newSpec.Spec.PgVersion { // PG versions comparison
		c.logger.Warnf("Postgresql version change(%s -> %s) is not allowed",
			c.Spec.PgVersion, newSpec.Spec.PgVersion)
		//TODO: rewrite pg version in tpr spec
	}

	if cmp.rollingUpdate {
		c.logger.Infof("Rolling update is needed")
		// TODO: wait for actual streaming to the replica
		if err := c.recreatePods(); err != nil {
			c.setStatus(spec.ClusterStatusUpdateFailed)
			return fmt.Errorf("could not recreate pods: %v", err)
		}
		c.logger.Infof("Rolling update has been finished")
	}
	c.setStatus(spec.ClusterStatusRunning)

	return nil
}

// updateVolumes changes size of the persistent volumes
// backed by EBS. It checks the new size against the PVs to
// decide whcih volumes to update. When updating, it first
// changes the EBS and only then update the PV.
func (c *Cluster) updateVolumes(newVolume spec.Volume) error {
	newQuantity, err := resource.ParseQuantity(newVolume.Size)
	// value in Gigabytes
	newSize := newQuantity.ScaledValue(0) / (1073741824)

	pvs, err := c.listPersistentVolumes()
	if err != nil {
		return fmt.Errorf("could not list persistent volumes: %s", err)
	}
	ec2, err := volumes.ConnectToEC2()
	if err != nil {
		return fmt.Errorf("could not connect to EC2")
	}
	for _, pv := range pvs {
		cap := pv.Spec.Capacity[v1.ResourceStorage]
		if cap.ScaledValue(0)/(1073741824) != newSize {
			awsVolumeId, err := getAWSVolumeId(pv)
			if err != nil {
				return err
			}
			c.logger.Debugf("updating persistent volume %s to %d", pv.Name, newSize)
			if err := volumes.ResizeVolume(ec2, awsVolumeId, newSize); err != nil {
				return fmt.Errorf("could not resize EBS volume %s: %v", awsVolumeId, err)
			}
			c.logger.Debugf("resizing the filesystem on the volume %s", pv.Name)
			podName := getPodNameFromPersistentVolume(pv)
			if err := c.resizeVolumeFS(podName); err != nil {
				return fmt.Errorf("could not resize the filesystem on pod '%s': %v", podName, err)
			}
			c.logger.Debugf("filesystem resize successfull on volume %s", pv.Name)
			pv.Spec.Capacity[v1.ResourceStorage] = newQuantity
			c.logger.Debugf("updating persistent volume definition for volume %s", pv.Name)
			if _, err := c.KubeClient.PersistentVolumes().Update(pv); err != nil {
				return fmt.Errorf("could not update persistent volume: %s", err)
			}
			c.logger.Debugf("successfully updated persistent volume %s", pv.Name)
		}
	}

	return nil
}

func (c *Cluster) resizeVolumeFS(podName *spec.NamespacedName) error {
	// resize2fs always writes to stderr, and ExecCommand considers a non-empty stderr an error
	out, err := c.ExecCommand(podName, "bash", "-c", "df -h /home/postgres/pgdata --output=source|tail -1|xargs resize2fs 2>&1")
	if err != nil {
		return err
	}

	if out != "" {
		c.logger.Debugf("command output is: %s", out)
	}

	if strings.Contains(out, "Nothing to do") ||
		(strings.Contains(out,  "on-line resizing required") && ext2fsSuccessRegexp.MatchString(out)) {
		return nil
	}
	return fmt.Errorf("unrecognized output: %s, assuming error", out)
}

func getPodNameFromPersistentVolume(pv *v1.PersistentVolume) *spec.NamespacedName {
	namespace := pv.Spec.ClaimRef.Namespace
	name := pv.Spec.ClaimRef.Name[len(constants.DataVolumeName)+1:]
	return &spec.NamespacedName{namespace, name}
}

// getAWSVolumeId converts aws://eu-central-1b/vol-00f93d4827217c629 to vol-00f93d4827217c629 for EBS volumes
func getAWSVolumeId(pv *v1.PersistentVolume) (string, error) {
	id := pv.Spec.AWSElasticBlockStore.VolumeID
	if id == "" {
		return "", fmt.Errorf("volume id is empty for volume %s", pv.Name)
	}
	idx := strings.LastIndex(id, "/vol-") + 1
	if idx == 0 {
		return "", fmt.Errorf("malfored EBS volume id %s", id)
	}
	return id[idx:], nil
}

func (c *Cluster) Delete() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.deleteEndpoint(); err != nil {
		return fmt.Errorf("could not delete endpoint: %v", err)
	}

	if err := c.deleteService(); err != nil {
		return fmt.Errorf("could not delete service: %v", err)
	}

	if err := c.deleteStatefulSet(); err != nil {
		return fmt.Errorf("could not delete statefulset: %v", err)
	}

	for _, obj := range c.Secrets {
		if err := c.deleteSecret(obj); err != nil {
			return fmt.Errorf("could not delete secret: %v", err)
		}
	}

	return nil
}

func (c *Cluster) ReceivePodEvent(event spec.PodEvent) {
	c.podEventsQueue.Add(event)
}

func (c *Cluster) processPodEvent(obj interface{}) error {
	event, ok := obj.(spec.PodEvent)
	if !ok {
		return fmt.Errorf("could not cast to PodEvent")
	}

	c.podSubscribersMu.RLock()
	subscriber, ok := c.podSubscribers[event.PodName]
	c.podSubscribersMu.RUnlock()
	if ok {
		subscriber <- event
	}

	return nil
}

func (c *Cluster) Run(stopCh <-chan struct{}) {
	go c.processPodEventQueue(stopCh)
}

func (c *Cluster) processPodEventQueue(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		default:
			c.podEventsQueue.Pop(cache.PopProcessFunc(c.processPodEvent))
		}
	}
}

func (c *Cluster) initSystemUsers() {
	// We don't actually use that to create users, delegating this
	// task to Patroni. Those definitions are only used to create
	// secrets, therefore, setting flags like SUPERUSER or REPLICATION
	// is not necessary here
	c.systemUsers[constants.SuperuserKeyName] = spec.PgUser{
		Name:     c.OpConfig.SuperUsername,
		Password: util.RandomPassword(constants.PasswordLength),
	}
	c.systemUsers[constants.ReplicationUserKeyName] = spec.PgUser{
		Name:     c.OpConfig.ReplicationUsername,
		Password: util.RandomPassword(constants.PasswordLength),
	}
}

func (c *Cluster) initRobotUsers() error {
	for username, userFlags := range c.Spec.Users {
		if !isValidUsername(username) {
			return fmt.Errorf("invalid username: '%v'", username)
		}

		flags, err := normalizeUserFlags(userFlags)
		if err != nil {
			return fmt.Errorf("invalid flags for user '%v': %v", username, err)
		}

		c.pgUsers[username] = spec.PgUser{
			Name:     username,
			Password: util.RandomPassword(constants.PasswordLength),
			Flags:    flags,
		}
	}

	return nil
}

func (c *Cluster) initHumanUsers() error {
	teamMembers, err := c.getTeamMembers()
	if err != nil {
		return fmt.Errorf("could not get list of team members: %v", err)
	}
	for _, username := range teamMembers {
		flags := []string{constants.RoleFlagLogin, constants.RoleFlagSuperuser}
		memberOf := []string{c.OpConfig.PamRoleName}
		c.pgUsers[username] = spec.PgUser{Name: username, Flags: flags, MemberOf: memberOf}
	}

	return nil
}

func (c *Cluster) initInfrastructureRoles() error {
	// add infrastucture roles from the operator's definition
	for username, data := range c.InfrastructureRoles {
		if !isValidUsername(username) {
			return fmt.Errorf("invalid username: '%v'", username)
		}
		flags, err := normalizeUserFlags(data.Flags)
		if err != nil {
			return fmt.Errorf("invalid flags for user '%v': %v", username, err)
		}
		data.Flags = flags
		c.pgUsers[username] = data
	}
	return nil
}
