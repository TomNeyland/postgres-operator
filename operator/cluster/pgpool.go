package cluster

/*
 Copyright 2017-2018 Crunchy Data Solutions, Inc.
 Licensed under the Apache License, Version 2.0 (the "License");
 you may not use this file except in compliance with the License.
 You may obtain a copy of the License at

      http://www.apache.org/licenses/LICENSE-2.0

 Unless required by applicable law or agreed to in writing, software
 distributed under the License is distributed on an "AS IS" BASIS,
 WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 See the License for the specific language governing permissions and
 limitations under the License.
*/

import (
	"bytes"
	"encoding/json"
	log "github.com/Sirupsen/logrus"
	crv1 "github.com/crunchydata/postgres-operator/apis/cr/v1"
	"github.com/crunchydata/postgres-operator/operator"
	"github.com/crunchydata/postgres-operator/util"
	"k8s.io/api/core/v1"
	//kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	//"k8s.io/apimachinery/pkg/watch"
	"crypto/md5"
	"encoding/hex"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"text/template"
)

type PgpoolPasswdFields struct {
	PG_USERNAME     string
	PG_PASSWORD_MD5 string
}

type PgpoolHBAFields struct {
}

type PgpoolConfFields struct {
	PG_PRIMARY_SERVICE_NAME string
	PG_REPLICA_SERVICE_NAME string
	PG_USERNAME             string
	PG_PASSWORD             string
}

type PgpoolTemplateFields struct {
	Name               string
	ClusterName        string
	SecretsName        string
	CCPImagePrefix     string
	CCPImageTag        string
	ContainerResources string
	Port               string
	PrimaryServiceName string
	ReplicaServiceName string
}

var pgpoolTemplate *template.Template
var pgpoolConfTemplate *template.Template
var pgpoolPasswdTemplate *template.Template
var pgpoolHBATemplate *template.Template

func init() {
	pgpoolTemplate = util.LoadTemplate("/operator-conf/pgpool-template.json")
	pgpoolConfTemplate = util.LoadTemplate("/operator-conf/pgpool.conf")
	pgpoolPasswdTemplate = util.LoadTemplate("/operator-conf/pool_passwd")
	pgpoolHBATemplate = util.LoadTemplate("/operator-conf/pool_hba.conf")

}

const PGPOOL_SUFFIX = "-pgpool"

// ProcessPgpool ...
func AddPgpool(clientset *kubernetes.Clientset, restclient *rest.RESTClient, cl *crv1.Pgcluster, namespace, secretName string) {
	var doc bytes.Buffer
	var err error

	clusterName := cl.Spec.Name
	pgpoolName := clusterName + PGPOOL_SUFFIX
	log.Debug("adding a pgpool " + pgpoolName)

	//create the pgpool deployment
	fields := PgpoolTemplateFields{
		Name:           pgpoolName,
		ClusterName:    clusterName,
		CCPImagePrefix: operator.CCPImagePrefix,
		CCPImageTag:    cl.Spec.CCPImageTag,
		Port:           "5432",
		SecretsName:    secretName,
	}

	err = pgpoolTemplate.Execute(&doc, fields)
	if err != nil {
		log.Error(err)
		return
	}
	log.Debug(doc.String())

	deployment := v1beta1.Deployment{}
	err = json.Unmarshal(doc.Bytes(), &deployment)
	if err != nil {
		log.Error("error unmarshalling pgpool json into Deployment " + err.Error())
		return
	}

	var deploymentResult *v1beta1.Deployment
	deploymentResult, err = clientset.ExtensionsV1beta1().Deployments(namespace).Create(&deployment)
	if err != nil {
		log.Error("error creating pgpool Deployment " + err.Error())
		return
	}
	log.Info("created pgpool Deployment " + deploymentResult.Name + " in namespace " + namespace)

	//create a service for the pgpool
	svcFields := ServiceTemplateFields{}
	svcFields.Name = pgpoolName
	svcFields.ClusterName = clusterName
	svcFields.Port = "5432"

	err = CreateService(clientset, &svcFields, namespace)
	if err != nil {
		log.Error(err)
		return
	}
}

// DeletePgpool
func DeletePgpool(clientset *kubernetes.Clientset, clusterName, namespace string) {

	var delProp meta_v1.DeletionPropagation
	delOptions := meta_v1.DeleteOptions{}
	delProp = meta_v1.DeletePropagationBackground
	delOptions.PropagationPolicy = &delProp
	pgpoolDepName := clusterName + "-pgpool"

	log.Debug("deleting pgpool deployment " + pgpoolDepName)

	err := clientset.ExtensionsV1beta1().Deployments(namespace).Delete(pgpoolDepName, &delOptions)
	if err != nil {
		log.Error("error deleting Deployment " + pgpoolDepName + err.Error())
	}

	//delete the service name=<clustename>-pgpool

	err = clientset.Core().Services(namespace).Delete(pgpoolDepName,
		&meta_v1.DeleteOptions{})
	if err != nil {
		log.Error("error deleting pgpool Service " + err.Error())
	} else {
		log.Info("deleted pgpool service " + pgpoolDepName)
	}

}

// CreatePgpoolSecret create a secret used by pgpool
func CreatePgpoolSecret(clientset *kubernetes.Clientset, primary, replica, db, secretName, username, password, namespace string) error {

	var err error
	var pgpoolHBABytes, pgpoolConfBytes, pgpoolPasswdBytes []byte

	pgpoolHBABytes, err = getPgpoolHBA()
	if err != nil {
		log.Error(err)
		return err
	}
	pgpoolConfBytes, err = getPgpoolConf(primary, replica, username, password)
	if err != nil {
		log.Error(err)
		return err
	}
	pgpoolPasswdBytes, err = getPgpoolPasswd(username, password)
	if err != nil {
		log.Error(err)
		return err
	}

	secret := v1.Secret{}

	secret.Name = secretName
	secret.ObjectMeta.Labels = make(map[string]string)
	secret.ObjectMeta.Labels["pg-database"] = db
	secret.ObjectMeta.Labels["pgpool"] = "true"
	secret.Data = make(map[string][]byte)
	secret.Data["pgpool.conf"] = pgpoolConfBytes
	secret.Data["pool_hba.conf"] = pgpoolHBABytes
	secret.Data["pool_passwd"] = pgpoolPasswdBytes

	_, err = clientset.Core().Secrets(namespace).Create(&secret)
	if err != nil {
		log.Error("error creating pgpool secret" + err.Error())
	} else {
		log.Debug("created pgpool secret " + secret.Name)
	}

	return err

}

func getPgpoolHBA() ([]byte, error) {
	var err error

	fields := PgpoolHBAFields{}

	var doc bytes.Buffer
	err = pgpoolHBATemplate.Execute(&doc, fields)
	if err != nil {
		log.Error(err)
		return doc.Bytes(), err
	}
	log.Debug(doc.String())

	return doc.Bytes(), err
}
func getPgpoolConf(primary, replica, username, password string) ([]byte, error) {
	var err error

	fields := PgpoolConfFields{}
	fields.PG_PRIMARY_SERVICE_NAME = primary
	fields.PG_REPLICA_SERVICE_NAME = replica
	fields.PG_USERNAME = username
	fields.PG_PASSWORD = password

	var doc bytes.Buffer
	err = pgpoolConfTemplate.Execute(&doc, fields)
	if err != nil {
		log.Error(err)
		return doc.Bytes(), err
	}
	log.Debug(doc.String())

	return doc.Bytes(), err
}
func getPgpoolPasswd(username, password string) ([]byte, error) {
	var err error

	fields := PgpoolPasswdFields{}
	fields.PG_USERNAME = username
	fields.PG_PASSWORD_MD5 = "md5" + GetMD5Hash(password+username)

	var doc bytes.Buffer
	err = pgpoolPasswdTemplate.Execute(&doc, fields)
	if err != nil {
		log.Error(err)
		return doc.Bytes(), err
	}
	log.Debug(doc.String())

	return doc.Bytes(), err
}
func GetMD5Hash(text string) string {
	hasher := md5.New()
	hasher.Write([]byte(text))
	return hex.EncodeToString(hasher.Sum(nil))
}