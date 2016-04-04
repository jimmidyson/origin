package util

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/openshift/origin/pkg/api/latest"
	"github.com/openshift/origin/pkg/client"
	serverapi "github.com/openshift/origin/pkg/cmd/server/api"
	"github.com/openshift/origin/pkg/template"
	kapi "k8s.io/kubernetes/pkg/api"
	kerrs "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/meta"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/runtime"
)

// NewJenkinsPipelineTemplate returns a new JenkinsPipelineTemplate.
func NewJenkinsPipelineTemplate(ns string, conf serverapi.JenkinsPipelineConfig, kubeClient *kclient.Client, osClient *client.Client) *JenkinsPipelineTemplate {
	return &JenkinsPipelineTemplate{
		Config:          conf,
		TargetNamespace: ns,
		kubeClient:      kubeClient,
		osClient:        osClient,
	}
}

// JenkinsPipelineTemplate stores the configuration of the
// JenkinsPipelineStrategy template, used to instantiate the Jenkins service in
// given namespace.
type JenkinsPipelineTemplate struct {
	Config          serverapi.JenkinsPipelineConfig
	TargetNamespace string
	kubeClient      *kclient.Client
	osClient        *client.Client
	items           []resourceMapping
	ProcessErrors   []error
	CreateErrors    []error
}

// Process processes the Jenkins template. If an error occurs
func (t *JenkinsPipelineTemplate) Process() *JenkinsPipelineTemplate {
	if len(t.items) > 0 {
		return t
	}
	jenkinsTemplate, err := t.osClient.Templates(t.Config.Namespace).Get(t.Config.TemplateName)
	if err != nil {
		if kerrs.IsNotFound(err) {
			t.ProcessErrors = append(t.ProcessErrors, fmt.Errorf("Jenkins pipeline template %s/%s not found", t.Config.Namespace, t.Config.TemplateName))
		} else {
			t.ProcessErrors = append(t.ProcessErrors, err)
		}
		return t
	}
	t.ProcessErrors = append(t.ProcessErrors, substituteTemplateParameters(jenkinsTemplate)...)
	pTemplate, err := t.osClient.TemplateConfigs(t.TargetNamespace).Create(jenkinsTemplate)
	if err != nil {
		t.ProcessErrors = append(t.ProcessErrors, fmt.Errorf("processing Jenkins template %s/%s failed: %v", t.Config.Namespace, t.Config.TemplateName, err))
		return t
	}
	var mappingErrs []error
	t.items, mappingErrs = mapJenkinsTemplateResources(pTemplate.Objects)
	if len(mappingErrs) > 0 {
		t.ProcessErrors = append(t.ProcessErrors, mappingErrs...)
		return t
	}
	glog.V(4).Infof("Processed Jenkins pipeline jenkinsTemplate %s/%s", pTemplate.Namespace, pTemplate.Namespace)
	return t
}

// injectUserVars injects user specified variables into the Template
func substituteTemplateParameters(t *templateapi.Template) []error {
	var errors []error
	for name, value := range values {
		if len(name) == 0 {
			errors = append(errors, fmt.Errorf("template parameter name cannot be empty (%q)", value))
			continue
		}
		if v := template.GetParameterByName(t, name); v != nil {
			v.Value = value
			v.Generate = ""
			template.AddParameter(t, *v)
		} else {
			errors = append(errors, fmt.Errorf("unknown parameter %q specified for template", name))
		}
	}
	return errors
}

// Instantiate instantiates the Jenkins template in the target namespace.
func (t *JenkinsPipelineTemplate) Instantiate() error {
	if len(t.Errors()) > 0 {
		return fmt.Errorf("unable to instantiate Jenkins, processing jenkins template failed")
	}
	if !t.hasJenkinsService() {
		err := fmt.Errorf("template %s/%s does not contain required service %q", t.Config.Namespace, t.Config.TemplateName, t.Config.ServiceName)
		t.CreateErrors = append(t.CreateErrors, err)
		return err
	}
	counter := 0
	for _, item := range t.items {
		var err error
		if item.IsOrigin {
			err = t.osClient.Post().Namespace(t.TargetNamespace).Resource(item.Resource).Body(item.RawJSON).Do().Error()
		} else {
			err = t.kubeClient.Post().Namespace(t.TargetNamespace).Resource(item.Resource).Body(item.RawJSON).Do().Error()
		}
		if err != nil {
			t.CreateErrors = append(t.CreateErrors, fmt.Errorf("creating Jenkins component %s/%s failed: %v", item.Kind, item.Name, err))
			continue
		}
		counter++
	}
	delta := len(t.items) - counter
	if delta != 0 {
		// TODO: Shold we rollback in this case?
		return fmt.Errorf("%d Jenkins pipeline components failed to create", delta)
	}
	return nil
}

// Errors returns the list of processing and creation errors.
func (t *JenkinsPipelineTemplate) Errors() []error {
	return append(t.ProcessErrors, t.CreateErrors...)
}

// resourceMapping specify resource metadata informations and JSON for items
// contained in the Jenkins template.
type resourceMapping struct {
	Name     string
	Kind     string
	Resource string
	RawJSON  []byte
	IsOrigin bool
}

// hasJenkinsService searches the template items and return true if the expected
// Jenkins service is contained in template.
func (t *JenkinsPipelineTemplate) hasJenkinsService() bool {
	if len(t.Errors()) > 0 {
		return false
	}
	for _, item := range t.items {
		if item.Name == t.Config.ServiceName && item.Kind == "Service" {
			return true
		}
	}
	return false
}

// jenkinsTemplateResourcesToMap converts the input runtime.Object provided by
// processed Jenkins template into a resource mappings ready for creation.
func mapJenkinsTemplateResources(input []runtime.Object) ([]resourceMapping, []error) {
	result := make([]resourceMapping, len(input))
	var resultErrs []error
	accessor := meta.NewAccessor()
	for index, item := range input {
		rawObj, ok := item.(*runtime.Unknown)
		if !ok {
			resultErrs = append(resultErrs, fmt.Errorf("unable to convert %+v to unknown object", item))
			continue
		}
		obj, err := runtime.Decode(kapi.Codecs.UniversalDecoder(), rawObj.RawJSON)
		if err != nil {
			resultErrs = append(resultErrs, fmt.Errorf("unable to decode %q", rawObj.RawJSON))
			continue
		}
		kind, err := kapi.Scheme.ObjectKind(obj)
		if err != nil {
			resultErrs = append(resultErrs, fmt.Errorf("unknown kind %+v ", obj))
			continue
		}
		plural, _ := meta.KindToResource(kind)
		name, err := accessor.Name(obj)
		if err != nil {
			resultErrs = append(resultErrs, fmt.Errorf("unknown name %+v ", obj))
			continue
		}
		result[index] = resourceMapping{
			Name:     name,
			Kind:     kind.Kind,
			Resource: plural.Resource,
			RawJSON:  rawObj.RawJSON,
			IsOrigin: latest.IsKindInAnyOriginGroup(kind.Kind),
		}
	}
	return result, resultErrs
}
