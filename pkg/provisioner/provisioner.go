package provisioner

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/supergiant/control/pkg/clouds"
	"github.com/supergiant/control/pkg/model"
	"github.com/supergiant/control/pkg/node"
	"github.com/supergiant/control/pkg/pki"
	"github.com/supergiant/control/pkg/profile"
	"github.com/supergiant/control/pkg/sgerrors"
	"github.com/supergiant/control/pkg/storage"
	"github.com/supergiant/control/pkg/util"
	"github.com/supergiant/control/pkg/workflows"
	"github.com/supergiant/control/pkg/workflows/steps"
)

const keySize = 4096

type KubeService interface {
	Create(ctx context.Context, k *model.Kube) error
	Get(ctx context.Context, name string) (*model.Kube, error)
}

type TaskProvisioner struct {
	kubeService  KubeService
	repository   storage.Interface
	getWriter    func(string) (io.WriteCloser, error)
	provisionMap map[clouds.Name]workflows.WorkflowSet
	// NOTE(stgleb): Since provisioner is shared object among all users of SG
	// this rate limiter will affect all users not allowing them to spin-up
	// to many instances at once, probably we may split rate limiter per user
	// in future to avoid interference between them.
	rateLimiter *RateLimiter

	// Cancel map - map of KubeID -> cancel function
	// that cancels
	cancelMap map[string]func()
}

func NewProvisioner(repository storage.Interface, kubeService KubeService,
	spawnInterval time.Duration) *TaskProvisioner {
	return &TaskProvisioner{
		kubeService: kubeService,
		repository:  repository,
		provisionMap: map[clouds.Name]workflows.WorkflowSet{
			clouds.DigitalOcean: {
				ProvisionMaster: workflows.DigitalOceanMaster,
				ProvisionNode:   workflows.DigitalOceanNode,
			},
			clouds.AWS: {
				ProvisionMaster: workflows.AWSMaster,
				ProvisionNode:   workflows.AWSNode,
				PreProvision:    workflows.AWSPreProvision,
			},
			clouds.GCE: {
				ProvisionMaster: workflows.GCEMaster,
				ProvisionNode:   workflows.GCENode,
			},
		},
		getWriter:   util.GetWriter,
		rateLimiter: NewRateLimiter(spawnInterval),
		cancelMap:   make(map[string]func()),
	}
}

// ProvisionCluster runs provisionCluster process among nodes
// that have been provided for provisionCluster
func (tp *TaskProvisioner) ProvisionCluster(parentContext context.Context,
	profile *profile.Profile, config *steps.Config) (map[string][]*workflows.Task, error) {
	masterTasks, nodeTasks, preProvisionTask, clusterTask := tp.prepare(config.Provider, len(profile.MasterProfiles),
		len(profile.NodesProfiles))

	// Get clusterID from taskID
	if clusterTask != nil && len(clusterTask.ID) >= 8 {
		config.ClusterID = clusterTask.ID[:8]
	} else {
		return nil, errors.New(fmt.Sprintf("Wrong value of cluster task %v", clusterTask))
	}

	// Save cancel that cancel cluster provisioning to cancelMap
	ctx, cancel := context.WithCancel(parentContext)
	tp.cancelMap[config.ClusterID] = cancel

	// TODO(stgleb): Make node names from task id before provisioning starts
	masters, nodes := nodesFromProfile(config.ClusterName,
		masterTasks, nodeTasks, profile)

	if err := bootstrapKeys(config); err != nil {
		return nil, errors.Wrap(err, "bootstrap keys")
	}

	if err := bootstrapCerts(config); err != nil {
		return nil, errors.Wrap(err, "bootstrap certs")
	}

	// Gather all task ids
	taskIds := grabTaskIds(preProvisionTask, clusterTask, masterTasks, nodeTasks)
	// Save cluster before provisioning
	err := tp.buildInitialCluster(ctx, profile, masters, nodes, config, taskIds)

	if err != nil {
		return nil, errors.Wrap(err, "build initial cluster")
	}

	// monitor cluster state in separate goroutine
	go tp.monitorClusterState(ctx, config.ClusterID, config.NodeChan(),
		config.KubeStateChan(), config.ConfigChan())

	go func() {
		var preProvisionErr error

		if preProvisionTask != nil {
			if preProvisionErr := tp.preProvision(ctx, preProvisionTask, config); preProvisionErr != nil {
				logrus.Errorf("Pre provisioning cluster %v", err)
			}

			// In case of preprovision failure stop provisioning process.
			if preProvisionErr != nil {
				logrus.Errorf("pre provision has failed with %v", err)
				return
			}

			// Copy config from preProvision task because it contains all things need for further
			// provisioning VPC, SecGroup, Subnets etc.
			config = preProvisionTask.Config
		}

		config.ReadyForBootstrapLatch = &sync.WaitGroup{}
		config.ReadyForBootstrapLatch.Add(len(profile.MasterProfiles))
		// ProvisionCluster masters and wait until n/2 + 1 of masters with etcd are up and running
		doneChan, failChan, err := tp.provisionMasters(ctx, profile, config, masterTasks)

		if err != nil {
			logrus.Errorf("ProvisionCluster master %v", err)
		}

		select {
		case <-ctx.Done():
			logrus.Errorf("Master cluster has not been created %v", ctx.Err())
			return
		case <-doneChan:
		case <-failChan:
			config.KubeStateChan() <- model.StateFailed
			logrus.Errorf("master cluster deployment has been failed")
			return
		}

		// Save cluster state when masters are provisioned
		logrus.Infof("master provisioning for cluster %s has finished successfully", config.ClusterID)

		// ProvisionCluster nodes
		tp.provisionNodes(ctx, profile, config, nodeTasks)

		// Wait for cluster checks are finished
		tp.waitCluster(ctx, clusterTask, config)
		logrus.Infof("cluster %s deployment has finished", config.ClusterID)
	}()

	taskMap := map[string][]*workflows.Task{
		"master":  masterTasks,
		"node":    nodeTasks,
		"cluster": {clusterTask},
	}

	if preProvisionTask != nil {
		taskMap["preprovision"] = []*workflows.Task{preProvisionTask}
	}

	return taskMap, nil
}

func (tp *TaskProvisioner) ProvisionNodes(parentContext context.Context, nodeProfiles []profile.NodeProfile, kube *model.Kube, config *steps.Config) ([]string, error) {
	if len(kube.Masters) != 0 {
		for key := range kube.Masters {
			config.AddMaster(kube.Masters[key])
		}
	} else {
		return nil, errors.Wrap(sgerrors.ErrNotFound, "master node")
	}

	// Save cancel function that cancels node provisioning to cancelMap
	ctx, cancel := context.WithCancel(parentContext)
	tp.cancelMap[config.ClusterID] = cancel

	if err := tp.loadCloudSpecificData(ctx, config); err != nil {
		return nil, errors.Wrap(err, "load cloud specific config")
	}

	providerWorkflowSet, ok := tp.provisionMap[config.Provider]

	if !ok {
		return nil, errors.Wrap(sgerrors.ErrNotFound, "provider workflow")
	}

	// monitor cluster state in separate goroutine
	go tp.monitorClusterState(ctx, config.ClusterID,
		config.NodeChan(), config.KubeStateChan(), config.ConfigChan())

	tasks := make([]string, 0, len(nodeProfiles))

	for _, nodeProfile := range nodeProfiles {
		// Protect cloud API with rate limiter
		tp.rateLimiter.Take()

		// Take node workflow for the provider
		t, err := workflows.NewTask(providerWorkflowSet.ProvisionNode, tp.repository)
		tasks = append(tasks, t.ID)

		if err != nil {
			return nil, errors.Wrap(sgerrors.ErrNotFound, "workflow")
		}

		fileName := util.MakeFileName(t.ID)
		writer, err := tp.getWriter(fileName)

		if err != nil {
			return nil, errors.Wrap(err, "get writer")
		}

		err = FillNodeCloudSpecificData(config.Provider, nodeProfile, config)

		if err != nil {
			return nil, errors.Wrap(err, "fill node profile data to config")
		}

		// Put task id to config so that create instance step can use this id when generate node name
		config.TaskID = t.ID
		errChan := t.Run(ctx, *config, writer)

		go func(cfg *steps.Config, errChan chan error) {
			err = <-errChan

			if err != nil {
				logrus.Errorf("add node to cluster %s caused an error %v", kube.ID, err)
				return
			}
		}(config, errChan)
	}

	return tasks, nil
}

func (tp *TaskProvisioner) Cancel(clusterID string) error {
	if cancelFunc := tp.cancelMap[clusterID]; cancelFunc != nil {
		cancelFunc()
	} else {
		return sgerrors.ErrNotFound
	}

	return nil
}

// prepare creates all tasks for provisioning according to cloud provider
func (tp *TaskProvisioner) prepare(name clouds.Name, masterCount, nodeCount int) ([]*workflows.Task, []*workflows.Task, *workflows.Task, *workflows.Task) {
	var (
		preProvisionTask  *workflows.Task
		postProvisionTask *workflows.Task
		err               error
	)

	masterTasks := make([]*workflows.Task, 0, masterCount)
	nodeTasks := make([]*workflows.Task, 0, nodeCount)
	//some clouds (e.g. AWS) requires running tasks before provisioning nodes (creating a VPC, Subnets, SecGroups, etc)
	switch name {
	case clouds.AWS:
		preProvisionTask, err = workflows.NewTask(tp.provisionMap[name].PreProvision, tp.repository)
		// We can't go further without pre provision task
		if err != nil {
			logrus.Errorf("create pre provision task has finished with %v", err)
			return nil, nil, nil, nil
		}
	case clouds.GCE:
	case clouds.DigitalOcean:
		// TODO(stgleb): Create key pairs here
	}

	for i := 0; i < masterCount; i++ {
		t, err := workflows.NewTask(tp.provisionMap[name].ProvisionMaster, tp.repository)

		if err != nil {
			logrus.Errorf("Task type %s not found", tp.provisionMap[name].ProvisionMaster)
			continue
		}
		masterTasks = append(masterTasks, t)
	}

	for i := 0; i < nodeCount; i++ {
		t, err := workflows.NewTask(tp.provisionMap[name].ProvisionNode, tp.repository)

		if err != nil {
			logrus.Errorf("Task type %s not found", tp.provisionMap[name].ProvisionNode)
			continue
		}
		nodeTasks = append(nodeTasks, t)
	}

	postProvisionTask, _ = workflows.NewTask(workflows.Cluster, tp.repository)

	return masterTasks, nodeTasks, preProvisionTask, postProvisionTask
}

// preProvision is for preparing activities before instances can be creates like
// creation of VPC, key pairs, security groups, subnets etc.
func (tp *TaskProvisioner) preProvision(ctx context.Context, preProvisionTask *workflows.Task, config *steps.Config) error {
	fileName := util.MakeFileName(preProvisionTask.ID)
	out, err := tp.getWriter(fileName)

	if err != nil {
		logrus.Errorf("Error getting writer for %s", fileName)
		return err
	}

	result := preProvisionTask.Run(ctx, *config, out)
	err = <-result

	if err != nil {
		config.KubeStateChan() <- model.StateFailed
		logrus.Errorf("pre provision task %s has finished with error %v",
			preProvisionTask.ID, err)
	} else {
		// Update kube state
		config.KubeStateChan() <- model.StateProvisioning
		// Update cloud spec
		config.ConfigChan() <- preProvisionTask.Config
		logrus.Infof("pre provision %s has finished", preProvisionTask.ID)
	}

	return err
}

func (tp *TaskProvisioner) provisionMasters(ctx context.Context, profile *profile.Profile, config *steps.Config, tasks []*workflows.Task) (chan struct{}, chan struct{}, error) {
	config.IsMaster = true
	doneChan := make(chan struct{})
	failChan := make(chan struct{})

	if len(profile.MasterProfiles) == 0 {
		close(doneChan)
		return doneChan, failChan, nil
	}
	// master latch controls when the majority of masters with etcd are up and running
	// so etcd is available for writes of flannel that starts on each machine
	masterLatch := util.NewCountdownLatch(ctx, len(profile.MasterProfiles)/2+1)

	// If we fail n /2 of master deploy jobs - all cluster deployment is failed
	failLatch := util.NewCountdownLatch(ctx, len(profile.MasterProfiles)/2+1)

	// ProvisionCluster master nodes
	for index, masterTask := range tasks {
		// Take token that allows perform action with Cloud Provider API
		tp.rateLimiter.Take()

		if masterTask == nil {
			logrus.Fatal(tasks)
		}
		fileName := util.MakeFileName(masterTask.ID)
		out, err := tp.getWriter(fileName)

		if err != nil {
			logrus.Errorf("Error getting writer for %s", fileName)
			return nil, nil, err
		}

		// Fulfill task config with data about provider specific node configuration
		p := profile.MasterProfiles[index]
		FillNodeCloudSpecificData(profile.Provider, p, config)

		go func(t *workflows.Task) {
			// Put task id to config so that create instance step can use this id when generate node name
			config.TaskID = t.ID
			result := t.Run(ctx, *config, out)
			err = <-result

			if err != nil {
				failLatch.CountDown()
				logrus.Errorf("master task %s has finished with error %v", t.ID, err)
			} else {
				masterLatch.CountDown()
				logrus.Infof("master-task %s has finished", t.ID)
			}
		}(masterTask)
	}

	go func() {
		masterLatch.Wait()
		close(doneChan)
	}()

	go func() {
		failLatch.Wait()
		close(failChan)
	}()

	return doneChan, failChan, nil
}

func (tp *TaskProvisioner) provisionNodes(ctx context.Context, profile *profile.Profile, config *steps.Config, tasks []*workflows.Task) {
	config.IsMaster = false
	config.ManifestConfig.IsMaster = false
	// Do internal communication inside private network
	if master := config.GetMaster(); master != nil {
		config.FlannelConfig.EtcdHost = master.PrivateIp
	} else {
		return
	}

	// ProvisionCluster nodes
	for index, nodeTask := range tasks {
		// Take token that allows perform action with Cloud Provider API
		tp.rateLimiter.Take()

		fileName := util.MakeFileName(nodeTask.ID)
		out, err := tp.getWriter(fileName)

		if err != nil {
			logrus.Errorf("Error getting writer for %s", fileName)
			return
		}

		// Fulfill task config with data about provider specific node configuration
		p := profile.NodesProfiles[index]
		FillNodeCloudSpecificData(profile.Provider, p, config)

		go func(t *workflows.Task) {
			// Put task id to config so that create instance step can use this id when generate node name
			config.TaskID = t.ID
			result := t.Run(ctx, *config, out)
			err = <-result

			if err != nil {
				logrus.Errorf("node task %s has finished with error %v", t.ID, err)
			} else {
				logrus.Infof("node-task %s has finished", t.ID)
			}
		}(nodeTask)
	}
}

func (tp *TaskProvisioner) waitCluster(ctx context.Context, clusterTask *workflows.Task, config *steps.Config) {
	// clusterWg controls entire cluster deployment, waits until all final checks are done
	clusterWg := sync.WaitGroup{}
	clusterWg.Add(1)

	fileName := util.MakeFileName(clusterTask.ID)
	out, err := tp.getWriter(fileName)

	if err != nil {
		logrus.Errorf("Error getting writer for %s", fileName)
		return
	}

	go func(t *workflows.Task) {
		defer clusterWg.Done()
		cfg := *config

		if master := config.GetMaster(); master != nil {
			cfg.Node = *master
		} else {
			config.KubeStateChan() <- model.StateFailed
			logrus.Errorf("No master found, cluster deployment failed")
			return
		}

		result := t.Run(ctx, cfg, out)
		err = <-result

		if err != nil {
			config.KubeStateChan() <- model.StateFailed
			logrus.Errorf("cluster task %s has finished with error %v", t.ID, err)
		} else {
			config.KubeStateChan() <- model.StateOperational
			logrus.Infof("cluster-task %s has finished", t.ID)
		}
	}(clusterTask)

	// Wait for all task to be finished
	clusterWg.Wait()
}

func (tp *TaskProvisioner) buildInitialCluster(ctx context.Context,
	profile *profile.Profile, masters, nodes map[string]*node.Node,
	config *steps.Config, taskIds []string) error {

	cluster := &model.Kube{
		ID:                  config.ClusterID,
		State:               model.StateProvisioning,
		Name:                config.ClusterName,
		Provider:            profile.Provider,
		AccountName:         config.CloudAccountName,
		RBACEnabled:         profile.RBACEnabled,
		ServicesCIDR:        profile.K8SServicesCIDR,
		Region:              profile.Region,
		Zone:                profile.Zone,
		SshUser:             config.SshConfig.User,
		SshPublicKey:        []byte(config.SshConfig.PublicKey),
		BootstrapPublicKey:  []byte(config.SshConfig.BootstrapPublicKey),
		BootstrapPrivateKey: []byte(config.SshConfig.BootstrapPrivateKey),
		User:                profile.User,
		Password:            profile.Password,

		Auth: model.Auth{
			Username:  config.CertificatesConfig.Username,
			Password:  config.CertificatesConfig.Password,
			CACert:    config.CertificatesConfig.CACert,
			CAKey:     config.CertificatesConfig.CAKey,
			AdminCert: config.CertificatesConfig.AdminCert,
			AdminKey:  config.CertificatesConfig.AdminKey,
		},

		Arch:                   profile.Arch,
		OperatingSystem:        profile.OperatingSystem,
		OperatingSystemVersion: profile.UbuntuVersion,
		K8SVersion:             profile.K8SVersion,
		DockerVersion:          profile.DockerVersion,
		HelmVersion:            profile.HelmVersion,
		Networking: model.Networking{
			Manager: profile.FlannelVersion,
			Version: profile.FlannelVersion,
			Type:    profile.NetworkType,
			CIDR:    profile.CIDR,
		},

		CloudSpec: profile.CloudSpecificSettings,
		Masters:   masters,
		Nodes:     nodes,
		Tasks:     taskIds,
	}

	return tp.kubeService.Create(ctx, cluster)
}

func (t *TaskProvisioner) updateCloudSpecificData(k *model.Kube, config *steps.Config) {
	logrus.Debugf("Update cloud specific data for kube %s",
		config.ClusterID)

	cloudSpecificSettings := make(map[string]string)

	// Load key data
	k.BootstrapPrivateKey = []byte(config.SshConfig.BootstrapPrivateKey)
	k.SshPublicKey = []byte(config.SshConfig.PublicKey)

	// Save cloudSpecificData in kube
	switch config.Provider {
	case clouds.AWS:
		// Save az to subnets mapping for this cluster
		k.Subnets = config.AWSConfig.Subnets
		// Copy data got from pre provision step to cloud specific settings of kube
		cloudSpecificSettings[clouds.AwsAZ] = config.AWSConfig.AvailabilityZone
		cloudSpecificSettings[clouds.AwsVpcCIDR] = config.AWSConfig.VPCCIDR
		cloudSpecificSettings[clouds.AwsVpcID] = config.AWSConfig.VPCID
		cloudSpecificSettings[clouds.AwsKeyPairName] = config.AWSConfig.KeyPairName
		cloudSpecificSettings[clouds.AwsMastersSecGroupID] =
			config.AWSConfig.MastersSecurityGroupID
		cloudSpecificSettings[clouds.AwsNodesSecgroupID] =
			config.AWSConfig.NodesSecurityGroupID
		// TODO(stgleb): this must be done for all types of clouds
		cloudSpecificSettings[clouds.AwsSshBootstrapPrivateKey] =
			config.SshConfig.BootstrapPrivateKey
		cloudSpecificSettings[clouds.AwsUserProvidedSshPublicKey] =
			config.SshConfig.PublicKey
		cloudSpecificSettings[clouds.AwsRouteTableID] =
			config.AWSConfig.RouteTableID
		cloudSpecificSettings[clouds.AwsInternetGateWayID] =
			config.AWSConfig.InternetGatewayID
		cloudSpecificSettings[clouds.AwsMasterInstanceProfile] =
			config.AWSConfig.MastersInstanceProfile
		cloudSpecificSettings[clouds.AwsNodeInstanceProfile] =
			config.AWSConfig.NodesInstanceProfile
		cloudSpecificSettings[clouds.AwsImageID] =
			config.AWSConfig.ImageID
	case clouds.GCE:
		// GCE is the most simple :-)
	case clouds.DigitalOcean:
		// DO deletes key by fingerprint that's why we need to download
		//this bootstrap public key
		k.BootstrapPublicKey = []byte(config.SshConfig.BootstrapPublicKey)
	}

	k.CloudSpec = cloudSpecificSettings
}

func (t *TaskProvisioner) loadCloudSpecificData(ctx context.Context, config *steps.Config) error {
	k, err := t.kubeService.Get(ctx, config.ClusterID)

	if err != nil {
		logrus.Errorf("get kube caused %v", err)
		return err
	}

	return util.LoadCloudSpecificDataFromKube(k, config)
}

// Create bootstrap key pair and save to config ssh section
func bootstrapKeys(config *steps.Config) error {
	private, public, err := generateKeyPair(keySize)

	if err != nil {
		return err
	}

	config.SshConfig.BootstrapPrivateKey = private
	config.SshConfig.BootstrapPublicKey = public

	return nil
}

func bootstrapCerts(config *steps.Config) error {
	ca, err := pki.NewCAPair(config.CertificatesConfig.ParenCert)
	if err != nil {
		return errors.Wrap(err, "bootstrap CA for provisioning")
	}
	config.CertificatesConfig.CACert = string(ca.Cert)
	config.CertificatesConfig.CAKey = string(ca.Key)

	admin, err := pki.NewAdminPair(ca)
	if err != nil {
		return errors.Wrap(err, "create admin certificates")
	}
	config.CertificatesConfig.AdminCert = string(admin.Cert)
	config.CertificatesConfig.AdminKey = string(admin.Key)

	return nil
}

// All cluster state changes during provisioning must be made in this function
func (tp *TaskProvisioner) monitorClusterState(ctx context.Context,
	clusterID string, nodeChan chan node.Node, kubeStateChan chan model.KubeState,
	configChan chan *steps.Config) {
	for {
		select {
		case n := <-nodeChan:
			k, err := tp.kubeService.Get(ctx, clusterID)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}

			if n.Role == node.RoleMaster {
				k.Masters[n.Name] = &n
			} else {
				k.Nodes[n.Name] = &n
			}

			err = tp.kubeService.Create(ctx, k)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}
		case state := <-kubeStateChan:
			logrus.Debugf("monitor: get kube %s", clusterID)
			k, err := tp.kubeService.Get(ctx, clusterID)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}

			k.State = state
			logrus.Debugf("monitor: update kube %s with state %s",
				k.ID, state)
			err = tp.kubeService.Create(ctx, k)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}
		case config := <-configChan:
			logrus.Debugf("monitor: get kube %s", clusterID)
			k, err := tp.kubeService.Get(ctx, clusterID)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}

			tp.updateCloudSpecificData(k, config)

			err = tp.kubeService.Create(ctx, k)

			if err != nil {
				logrus.Errorf("cluster monitor: update kube state caused %v", err)
				continue
			}
		case <-ctx.Done():
			return
		}
	}
}
