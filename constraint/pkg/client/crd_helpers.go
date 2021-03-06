package client

import (
	"errors"
	"fmt"
	"strings"

	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1alpha1"
	"github.com/open-policy-agent/frameworks/constraint/pkg/apis/templates/v1beta1"
	"github.com/open-policy-agent/frameworks/constraint/pkg/core/templates"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

var supportedVersions = map[string]bool{
	v1alpha1.SchemeGroupVersion.Version: true,
	v1beta1.SchemeGroupVersion.Version:  true,
}

// validateTargets ensures that the targets field has the appropriate values
func validateTargets(templ *templates.ConstraintTemplate) error {
	if len(templ.Spec.Targets) > 1 {
		return errors.New("Multi-target templates are not currently supported")
	} else if templ.Spec.Targets == nil {
		return errors.New(`Field "targets" not specified in ConstraintTemplate spec`)
	} else if len(templ.Spec.Targets) == 0 {
		return errors.New("No targets specified. ConstraintTemplate must specify one target")
	}
	return nil
}

// createSchema combines the schema of the match target and the ConstraintTemplate parameters
// to form the schema of the actual constraint resource
func (h *crdHelper) createSchema(templ *templates.ConstraintTemplate, target MatchSchemaProvider) (*apiextensions.JSONSchemaProps, error) {
	props := map[string]apiextensions.JSONSchemaProps{
		"match":             target.MatchSchema(),
		"enforcementAction": apiextensions.JSONSchemaProps{Type: "string"},
	}
	if templ.Spec.CRD.Spec.Validation != nil && templ.Spec.CRD.Spec.Validation.OpenAPIV3Schema != nil {
		internalSchema := &apiextensions.JSONSchemaProps{}
		if err := h.scheme.Convert(templ.Spec.CRD.Spec.Validation.OpenAPIV3Schema, internalSchema, nil); err != nil {
			return nil, err
		}
		props["parameters"] = *internalSchema
	}
	schema := &apiextensions.JSONSchemaProps{
		Properties: map[string]apiextensions.JSONSchemaProps{
			"spec": apiextensions.JSONSchemaProps{
				Properties: props,
			},
		},
	}
	return schema, nil
}

// crdHelper builds the scheme for handling CRDs. It is necessary to build crdHelper at runtime as
// modules are added to the CRD scheme builder during the init stage
type crdHelper struct {
	scheme *runtime.Scheme
}

func newCRDHelper() (*crdHelper, error) {
	scheme := runtime.NewScheme()
	if err := apiextensionsv1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return &crdHelper{scheme: scheme}, nil
}

// createCRD takes a template and a schema and converts it to a CRD
func (h *crdHelper) createCRD(
	templ *templates.ConstraintTemplate,
	schema *apiextensions.JSONSchemaProps) (*apiextensions.CustomResourceDefinition, error) {
	crd := &apiextensions.CustomResourceDefinition{
		Spec: apiextensions.CustomResourceDefinitionSpec{
			Group: constraintGroup,
			Names: apiextensions.CustomResourceDefinitionNames{
				Kind:     templ.Spec.CRD.Spec.Names.Kind,
				ListKind: templ.Spec.CRD.Spec.Names.Kind + "List",
				Plural:   strings.ToLower(templ.Spec.CRD.Spec.Names.Kind),
				Singular: strings.ToLower(templ.Spec.CRD.Spec.Names.Kind),
				Categories: []string{
					"all",
					"constraint",
				},
			},
			Validation: &apiextensions.CustomResourceValidation{
				OpenAPIV3Schema: schema,
			},
			Scope:   "Cluster",
			Version: v1beta1.SchemeGroupVersion.Version,
			Versions: []apiextensions.CustomResourceDefinitionVersion{
				{
					Name:    v1beta1.SchemeGroupVersion.Version,
					Storage: true,
					Served:  true,
				},
				{
					Name:    v1alpha1.SchemeGroupVersion.Version,
					Storage: false,
					Served:  true,
				},
			},
		},
	}
	// Defaulting functions only exist for v1beta1
	v1b1 := &apiextensionsv1beta1.CustomResourceDefinition{}
	if err := h.scheme.Convert(crd, v1b1, nil); err != nil {
		return nil, err
	}
	h.scheme.Default(v1b1)
	crd2 := &apiextensions.CustomResourceDefinition{}
	if err := h.scheme.Convert(v1b1, crd2, nil); err != nil {
		return nil, err
	}
	crd2.ObjectMeta.Name = fmt.Sprintf("%s.%s", crd.Spec.Names.Plural, constraintGroup)
	return crd2, nil
}

// validateCRD calls the CRD package's validation on an internal representation of the CRD
func (h *crdHelper) validateCRD(crd *apiextensions.CustomResourceDefinition) error {
	errors := apiextensionsvalidation.ValidateCustomResourceDefinition(crd, apiextensionsv1beta1.SchemeGroupVersion)
	if len(errors) > 0 {
		return errors.ToAggregate()
	}
	return nil
}

// validateCR validates the provided custom resource against its CustomResourceDefinition
func (h *crdHelper) validateCR(cr *unstructured.Unstructured, crd *apiextensions.CustomResourceDefinition) error {
	validator, _, err := validation.NewSchemaValidator(crd.Spec.Validation)
	if err != nil {
		return err
	}
	if err := validation.ValidateCustomResource(field.NewPath(""), cr, validator); err != nil {
		return err.ToAggregate()
	}
	if errs := apivalidation.IsDNS1123Subdomain(cr.GetName()); len(errs) != 0 {
		return fmt.Errorf("Invalid Name: %s", strings.Join(errs, "\n"))
	}
	if cr.GetKind() != crd.Spec.Names.Kind {
		return fmt.Errorf("Wrong kind for constraint %s. Have %s, want %s", cr.GetName(), cr.GetKind(), crd.Spec.Names.Kind)
	}
	if cr.GroupVersionKind().Group != constraintGroup {
		return fmt.Errorf("Wrong group for constraint %s. Have %s, want %s", cr.GetName(), cr.GroupVersionKind().Group, constraintGroup)
	}
	if !supportedVersions[cr.GroupVersionKind().Version] {
		return fmt.Errorf("Wrong version for constraint %s. Have %s, supported: %v", cr.GetName(), cr.GroupVersionKind().Version, supportedVersions)
	}
	return nil
}
