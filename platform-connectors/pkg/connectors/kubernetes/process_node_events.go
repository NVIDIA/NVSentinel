package kubernetes

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"k8s.io/client-go/util/retry"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	DefaultNamespace   = "default"
	NoHealthFailureMsg = "No Health Failures"
	XidErrorCheck      = "XidError"
)

func (r *K8sConnector) updateNodeCondition(ctx context.Context, condition corev1.NodeCondition, isHealthy bool) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := r.clientset.CoreV1().Nodes().Get(ctx, r.nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Error getting node: %s", err)
			return err
		}

		conditionExists := false

		for i, c := range node.Status.Conditions {
			if c.Type == condition.Type {
				if condition.Status != c.Status {
					condition.LastTransitionTime = condition.LastHeartbeatTime
				}

				if !isHealthy {
					if c.Message != NoHealthFailureMsg && !strings.Contains(c.Message, condition.Message) {
						condition.Message = fmt.Sprintf("%s %s", c.Message, condition.Message)
					}
				}

				node.Status.Conditions[i] = condition
				conditionExists = true
			}
		}

		if !conditionExists {
			node.Status.Conditions = append(node.Status.Conditions, condition)
		}
		// Update the node status
		_, err = r.clientset.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{})
		if err == nil {
			klog.Infof("Node condition %s updated for node %s", condition.Type, r.nodeName)
		}

		return err
	})

	return err
}

func (r *K8sConnector) writeNodeEvent(ctx context.Context, event *corev1.Event) error {
	_, err := r.clientset.CoreV1().Events(DefaultNamespace).Create(ctx, event, metav1.CreateOptions{})
	if err != nil {
		klog.Infof("Error creating event: %s", err)
		return err
	}

	klog.Infof("Event created for node %s", r.nodeName)

	return nil
}

func (r *K8sConnector) fetchHealthEventReason(healthEvent *platformconnector.HealthEvent) string {
	reason := ""

	if healthEvent.CheckName == XidErrorCheck {
		switch healthEvent.IsHealthy {
		case true:
			reason = "NoXidErrorDetected"
		default:
			reason = "XidErrorDetected"
		}
	} else {
		if healthEvent.IsHealthy {
			reason = fmt.Sprintf("%sIsHealthy", healthEvent.CheckName)
		} else {
			reason = fmt.Sprintf("%sIsNotHealthy", healthEvent.CheckName)
		}
	}

	return reason
}

func (r *K8sConnector) fetchHealthEventMessage(healthEvent *platformconnector.HealthEvent) string {
	message := ""

	if healthEvent.IsHealthy {
		message = NoHealthFailureMsg
	} else {
		if healthEvent.CheckName == XidErrorCheck {
			message = "XID" + healthEvent.ErrorCode
		} else {
			message = healthEvent.ErrorCode
		}

		for _, entity := range healthEvent.EntitiesImpacted {
			message += ":" + entity
		}

		message += "."
	}

	return message
}

func (r *K8sConnector) processHealthEvents(ctx context.Context, healthEvent *platformconnector.HealthEvent) error {
	conditionType := corev1.NodeConditionType(string(healthEvent.CheckName))
	healthEventStatus := "True"

	if healthEvent.IsHealthy {
		healthEventStatus = "False"
	}

	reason := r.fetchHealthEventReason(healthEvent)
	message := r.fetchHealthEventMessage(healthEvent)

	klog.Infof("healthEvent checkType %s isHealthy %t isFatal %t",
		healthEvent.CheckName, healthEvent.IsHealthy, healthEvent.IsFatal)

	if healthEvent.IsHealthy || healthEvent.IsFatal {
		newCondition := corev1.NodeCondition{
			Type:               conditionType,
			Status:             corev1.ConditionStatus(healthEventStatus),
			LastHeartbeatTime:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
			LastTransitionTime: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
			Reason:             reason,
			Message:            message,
		}

		klog.Infof("updating node condition %s", conditionType)

		return r.updateNodeCondition(ctx, newCondition, healthEvent.IsHealthy)
	}

	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%x", r.nodeName, metav1.Now().UnixNano()),
			Namespace: DefaultNamespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Node",
			Name: r.nodeName,
			UID:  types.UID(r.nodeName),
		},
		Reason:              reason,
		ReportingController: healthEvent.Agent,
		ReportingInstance:   r.nodeName,
		Message:             message,
		Source: corev1.EventSource{
			Component: healthEvent.Agent,
			Host:      r.nodeName,
		},
		FirstTimestamp: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
		LastTimestamp:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
		Type:           healthEvent.CheckName,
	}

	if !healthEvent.IsHealthy {
		klog.Infof("healthEvent is not healthy.Writing event %s", healthEvent.CheckName)
		return r.writeNodeEvent(ctx, event)
	}

	klog.Infof("healhtEvent is healthy.Writing event %s", healthEvent.CheckName)

	return r.writeNodeEvent(ctx, event)
}
