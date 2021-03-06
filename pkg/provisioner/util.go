package provisioner

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/supergiant/control/pkg/clouds"
	"github.com/supergiant/control/pkg/node"
	"github.com/supergiant/control/pkg/profile"
	"github.com/supergiant/control/pkg/sgerrors"
	"github.com/supergiant/control/pkg/util"
	"github.com/supergiant/control/pkg/workflows"
	"github.com/supergiant/control/pkg/workflows/steps"
)

type RateLimiter struct {
	bucket *time.Ticker
}

func NewRateLimiter(interval time.Duration) *RateLimiter {
	return &RateLimiter{
		bucket: time.NewTicker(interval),
	}
}

// Take either returns giving calling code ability to execute or blocks until
// bucket is full again
func (r *RateLimiter) Take() {
	<-r.bucket.C
}

// Fill cloud account specific data gets data from the map and puts to particular cloud provider config
func FillNodeCloudSpecificData(provider clouds.Name, nodeProfile profile.NodeProfile, config *steps.Config) error {
	switch provider {
	case clouds.AWS:
		return util.BindParams(nodeProfile, &config.AWSConfig)
	case clouds.GCE:
		return util.BindParams(nodeProfile, &config.GCEConfig)
	case clouds.DigitalOcean:
		return util.BindParams(nodeProfile, &config.DigitalOceanConfig)
	case clouds.Packet:
		return util.BindParams(nodeProfile, &config.PacketConfig)
	case clouds.OpenStack:
		return util.BindParams(nodeProfile, &config.OSConfig)
	default:
		return sgerrors.ErrUnknownProvider
	}

	return nil
}

func nodesFromProfile(clusterName string, masterTasks, nodeTasks []*workflows.Task, profile *profile.Profile) (map[string]*node.Node, map[string]*node.Node) {
	masters := make(map[string]*node.Node)
	nodes := make(map[string]*node.Node)

	for index, p := range profile.MasterProfiles {
		taskId := masterTasks[index].ID
		name := util.MakeNodeName(clusterName, taskId, true)

		// TODO(stgleb): check if we can lowercase node names for all nodes
		if profile.Provider == clouds.GCE {
			name = strings.ToLower(name)
		}
		n := &node.Node{
			TaskID:   taskId,
			Name:     name,
			Provider: profile.Provider,
			Region:   profile.Region,
			State:    node.StatePlanned,
		}

		util.BindParams(p, n)
		masters[n.Name] = n
	}

	for index, p := range profile.NodesProfiles {
		taskId := nodeTasks[index].ID
		name := util.MakeNodeName(clusterName, taskId[:4], false)

		// TODO(stgleb): check if we can lowercase node names for all nodes
		if profile.Provider == clouds.GCE {
			name = strings.ToLower(name)
		}
		n := &node.Node{
			TaskID:   taskId,
			Name:     name,
			Provider: profile.Provider,
			Region:   profile.Region,
			State:    node.StatePlanned,
		}

		util.BindParams(p, n)
		nodes[n.Name] = n
	}

	return masters, nodes
}

func generateKeyPair(size int) (string, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, size)

	if err != nil {
		return "", "", err
	}

	privateKeyPem := encodePrivateKeyToPEM(privateKey)
	publicKey, err := generatePublicKey(&privateKey.PublicKey)

	if err != nil {
		return "", "", err
	}

	return string(privateKeyPem), string(publicKey), nil
}

// encodePrivateKeyToPEM encodes Private Key from RSA to PEM format
func encodePrivateKeyToPEM(privateKey *rsa.PrivateKey) []byte {
	// Get ASN.1 DER format
	privDER := x509.MarshalPKCS1PrivateKey(privateKey)

	// pem.Block
	privBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privDER,
	}

	// Private key in PEM format
	privatePEM := pem.EncodeToMemory(&privBlock)

	return privatePEM
}

func generatePublicKey(publicKey *rsa.PublicKey) ([]byte, error) {
	publicRsaKey, err := ssh.NewPublicKey(publicKey)

	if err != nil {
		return nil, err
	}

	pubKeyBytes := ssh.MarshalAuthorizedKey(publicRsaKey)

	return pubKeyBytes, nil
}

func grabTaskIds(preProvisionTask, clusterTask *workflows.Task, masterTasks, nodeTasks []*workflows.Task) []string {
	taskIds := make([]string, 0)
	taskIds = append(taskIds, clusterTask.ID)

	// NOTE(stgleb): not all providers have preProvision type of workflow
	if preProvisionTask != nil {
		taskIds = append(taskIds, preProvisionTask.ID)
	}

	for _, task := range masterTasks {
		taskIds = append(taskIds, task.ID)
	}

	for _, task := range nodeTasks {
		taskIds = append(taskIds, task.ID)
	}

	return taskIds
}
