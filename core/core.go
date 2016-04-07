package core

import (
	"log"

	"github.com/supergiant/guber"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elb"
)

type Core struct {
	DB          *DB
	K8S         *guber.Client
	EC2         *ec2.EC2
	ELB         *elb.ELB
	AutoScaling *autoscaling.AutoScaling
}

var (
	EtcdEndpoints []string
	K8sHost       string
	K8sUser       string
	K8sPass       string
	AwsRegion     string

	AwsAZ       string
	AwsSgID     string
	AwsSubnetID string
)

func New(httpsMode bool, aws_access_key_id string, aws_secret_access_key string) *Core {

	// If you're working with temporary security credentials,
	// you can also keep the session token in AWS_SESSION_TOKEN.
	// TODO: We need to set this up when we have more timez
	token := ""

	creds := credentials.NewStaticCredentials(aws_access_key_id, aws_secret_access_key, token)
	_, err := creds.Get()
	if err != nil {
		log.Println("ERROR: AWS Credentials Install Failed...", err)
	}
	log.Println("INFO: AWS Credentials Installed.")

	c := Core{}
	c.DB = NewDB(EtcdEndpoints)
	c.K8S = guber.NewClient(K8sHost, K8sUser, K8sPass, httpsMode)

	awsConf := aws.NewConfig().WithRegion(AwsRegion).WithCredentials(creds)

	c.EC2 = ec2.New(session.New(), awsConf.WithLogLevel(aws.LogDebug))
	c.ELB = elb.New(session.New(), awsConf.WithLogLevel(aws.LogDebug))
	c.AutoScaling = autoscaling.New(session.New(), awsConf.WithLogLevel(aws.LogDebug))
	return &c
}

// Top-level resources
//==============================================================================
func (c *Core) Apps() *AppCollection {
	return &AppCollection{c}
}

func (c *Core) Entrypoints() *EntrypointCollection {
	return &EntrypointCollection{c}
}

func (c *Core) ImageRepos() *ImageRepoCollection {
	return &ImageRepoCollection{c}
}

func (c *Core) Tasks() *TaskCollection {
	return &TaskCollection{c}
}