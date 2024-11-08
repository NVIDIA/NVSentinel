package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	platformconnector "gitlab-master.nvidia.com/dgxcloud/mk8s/k8s-addons/nvsentinel/platform-connectors/pkg/protos"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	DefaultNamespace   = "default"
	NoHealthFailureMsg = "No Health Failures"
)

//nolint:cyclop, gocognit
func (r *K8sConnector) updateNodeCondition(ctx context.Context, condition corev1.NodeCondition,
	healthEvent *platformconnector.HealthEvent) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := r.clientset.CoreV1().Nodes().Get(ctx, r.nodeName, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("Error getting node: %s", err)
			return err
		}

		conditionExists := false

		for i, c := range node.Status.Conditions {
			/// nolint:nestif
			if c.Type == condition.Type {
				conditionExists = true

				if condition.Status != c.Status {
					condition.LastTransitionTime = condition.LastHeartbeatTime
				}

				// split meesages by ";" in condition
				messages := r.parseMessages(c.Message)

				if !healthEvent.IsHealthy {
					// add the new message if it doesn't exist
					messages = r.addMessageIfNotExist(messages, condition.Message)

					c.Status = corev1.ConditionTrue
				} else {
					// remove messages that include any of the entities in entitiesImpacted, else if empty then clear the messages
					if len(healthEvent.EntitiesImpacted) > 0 {
						messages = r.removeImpactedEntitiesMessages(messages, healthEvent.EntitiesImpacted,
							healthEvent.CheckName)
					} else {
						messages = make([]string, 0)
					}
				}

				if len(messages) > 0 {
					c.Message = fmt.Sprintf("%s;", strings.Join(messages, ";"))
					c.Status = corev1.ConditionTrue
					c.Reason = r.updateHealthEventReason(healthEvent.CheckName, false)
				} else {
					c.Message = NoHealthFailureMsg
					c.Status = corev1.ConditionFalse
					c.Reason = r.updateHealthEventReason(healthEvent.CheckName, true)
				}

				c.LastHeartbeatTime = condition.LastHeartbeatTime
				c.LastTransitionTime = condition.LastTransitionTime

				node.Status.Conditions[i] = c

				break
			}
		}

		if !conditionExists {
			if !healthEvent.IsHealthy {
				condition.Status = corev1.ConditionTrue
			} else {
				condition.Status = corev1.ConditionFalse
				condition.Message = NoHealthFailureMsg
			}

			condition.Reason = r.updateHealthEventReason(healthEvent.CheckName, healthEvent.IsHealthy)

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

func (r *K8sConnector) parseMessages(message string) []string {
	var messages []string

	if message != "" && message != NoHealthFailureMsg {
		elementMessages := strings.Split(message, ";")
		for _, msg := range elementMessages {
			if msg != "" {
				messages = append(messages, msg)
			}
		}
	}

	return messages
}

func (r *K8sConnector) addMessageIfNotExist(messages []string, newMessage string) []string {
	for _, msg := range messages {
		if fmt.Sprintf("%s;", msg) == newMessage {
			return messages
		}
	}

	return append(messages, newMessage[:len(newMessage)-1])
}

func (r *K8sConnector) removeImpactedEntitiesMessages(messages []string, entities []*platformconnector.Entity,
	checkName string) []string {
	var newMessages []string

	for _, msg := range messages {
		entityFound := false

		for _, entity := range entities {
			entityPrefix := fmt.Sprintf("%s:%s", entity.EntityType, entity.EntityValue)

			if strings.Contains(msg, entityPrefix) {
				entityFound = true
				break
			}
		}

		if !entityFound {
			newMessages = append(newMessages, msg)
		}
	}

	return newMessages
}

func (r *K8sConnector) writeNodeEvent(ctx context.Context, event *corev1.Event) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Fetch all events for the node
		events, err := r.clientset.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{
			FieldSelector: fmt.Sprintf("involvedObject.name=%s", r.nodeName),
		})
		if err != nil {
			return err
		}

		// Check if any event matches the new event
		for _, existingEvent := range events.Items {
			if existingEvent.Type == event.Type && existingEvent.Reason == event.Reason &&
				existingEvent.Message == event.Message {
				// Matching event found, update it
				existingEvent.Count++
				existingEvent.LastTimestamp = event.LastTimestamp

				_, err = r.clientset.CoreV1().Events(DefaultNamespace).Update(ctx, &existingEvent, metav1.UpdateOptions{})
				if err != nil {
					nodeEventUpdateFailureCounter.Inc()
				} else {
					nodeEventUpdateSuccessCounter.Inc()
				}

				return err
			}
		}

		// No matching event found, create a new event with count 1
		event.Count = 1

		_, err = r.clientset.CoreV1().Events(DefaultNamespace).Create(ctx, event, metav1.CreateOptions{})
		if err != nil {
			nodeEventCreationFailureCounter.Inc()
		} else {
			nodeEventCreationSuccessCounter.Inc()
		}

		return err
	})

	return err
}

func (r *K8sConnector) updateHealthEventReason(checkName string, isHealthy bool) string {
	status := "IsNotHealthy"
	if isHealthy {
		status = "IsHealthy"
	}

	return fmt.Sprintf("%s%s", checkName, status)
}

func (r *K8sConnector) fetchHealthEventMessage(healthEvent *platformconnector.HealthEvent) string {
	message := ""

	if healthEvent.IsHealthy {
		message = NoHealthFailureMsg
	} else {
		for _, errorCode := range healthEvent.ErrorCode {
			message += fmt.Sprintf("ErrorCode:%s ", errorCode)
		}

		for _, entity := range healthEvent.EntitiesImpacted {
			message += fmt.Sprintf("%s:%s ", entity.EntityType, entity.EntityValue)
		}

		if healthEvent.Message != "" {
			message += fmt.Sprintf("%s ", healthEvent.Message)
		}

		message += fmt.Sprintf("Recommended Action=%s;", healthEvent.RecommendedAction.String())
	}

	return message
}

func (r *K8sConnector) processHealthEvents(ctx context.Context, healthEvent *platformconnector.HealthEvent) error {
	conditionType := corev1.NodeConditionType(string(healthEvent.CheckName))

	message := r.fetchHealthEventMessage(healthEvent)

	if healthEvent.IsHealthy || healthEvent.IsFatal {
		newCondition := corev1.NodeCondition{
			Type:               conditionType,
			LastHeartbeatTime:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
			LastTransitionTime: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
			Message:            message,
		}

		klog.Infof("updating node condition %s", conditionType)

		start := time.Now()

		err := r.updateNodeCondition(ctx, newCondition, healthEvent)

		duration := float64(time.Since(start).Milliseconds())
		nodeConditionUpdateDuration.Observe(duration)

		if err != nil {
			nodeConditionUpdateFailureCounter.Inc()
			return err
		}

		nodeConditionUpdateSuccessCounter.Inc()

		return nil
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
		Reason:              r.updateHealthEventReason(healthEvent.CheckName, healthEvent.IsHealthy),
		ReportingController: healthEvent.Agent,
		ReportingInstance:   r.nodeName,
		Message:             message,
		Count:               1,
		Source: corev1.EventSource{
			Component: healthEvent.Agent,
			Host:      r.nodeName,
		},
		FirstTimestamp: metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
		LastTimestamp:  metav1.NewTime(healthEvent.GeneratedTimestamp.AsTime()),
		Type:           healthEvent.CheckName,
	}

	if !healthEvent.IsHealthy {
		start := time.Now()

		err := r.writeNodeEvent(ctx, event)

		duration := float64(time.Since(start).Milliseconds())
		nodeEventUpdateCreateDuration.Observe(duration)

		if err != nil {
			return err
		}
	}

	return nil
}
