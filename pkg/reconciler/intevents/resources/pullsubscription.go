/*
Copyright 2019 Google LLC

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

package resources

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/kmeta"

	duckv1beta1 "github.com/google/knative-gcp/pkg/apis/duck/v1beta1"
	inteventsv1beta1 "github.com/google/knative-gcp/pkg/apis/intevents/v1beta1"
)

type PullSubscriptionArgs struct {
	Namespace   string
	Name        string
	Spec        *duckv1beta1.PubSubSpec
	Owner       kmeta.OwnerRefable
	Topic       string
	AdapterType string
	Mode        inteventsv1beta1.ModeType
	Labels      map[string]string
	Annotations map[string]string
}

// MakePullSubscription creates the spec for, but does not create, a GCP PullSubscription
// for a given GCS.
func MakePullSubscription(args *PullSubscriptionArgs) *inteventsv1beta1.PullSubscription {
	ps := &inteventsv1beta1.PullSubscription{
		ObjectMeta: metav1.ObjectMeta{
			Name:            args.Name,
			Namespace:       args.Namespace,
			Labels:          args.Labels,
			Annotations:     args.Annotations,
			OwnerReferences: []metav1.OwnerReference{*kmeta.NewControllerRef(args.Owner)},
		},
		Spec: inteventsv1beta1.PullSubscriptionSpec{
			PubSubSpec: duckv1beta1.PubSubSpec{
				IdentitySpec: duckv1beta1.IdentitySpec{
					ServiceAccountName: args.Spec.IdentitySpec.ServiceAccountName,
				},
				Secret:  args.Spec.Secret,
				Project: args.Spec.Project,
				SourceSpec: duckv1.SourceSpec{
					Sink: args.Spec.SourceSpec.Sink,
				},
			},
			Topic:       args.Topic,
			AdapterType: args.AdapterType,
			Mode:        args.Mode,
		},
	}
	if args.Spec.CloudEventOverrides != nil && args.Spec.CloudEventOverrides.Extensions != nil {
		ps.Spec.SourceSpec.CloudEventOverrides = &duckv1.CloudEventOverrides{
			Extensions: args.Spec.CloudEventOverrides.Extensions,
		}
	}
	return ps
}
