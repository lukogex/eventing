/*
Copyright 2020 The Knative Authors

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

package subscription

import (
	"context"
	"fmt"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"knative.dev/pkg/apis"

	"knative.dev/pkg/apis/duck"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/kref"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/resolver"
	"knative.dev/pkg/tracker"

	eventingduckv1 "knative.dev/eventing/pkg/apis/duck/v1"
	"knative.dev/eventing/pkg/apis/feature"
	v1 "knative.dev/eventing/pkg/apis/messaging/v1"
	subscriptionreconciler "knative.dev/eventing/pkg/client/injection/reconciler/messaging/v1/subscription"
	listers "knative.dev/eventing/pkg/client/listers/messaging/v1"
	eventingduck "knative.dev/eventing/pkg/duck"
)

const (
	// Name of the corev1.Events emitted from the reconciliation process
	subscriptionUpdateStatusFailed      = "UpdateFailed"
	physicalChannelSyncFailed           = "PhysicalChannelSyncFailed"
	subscriptionNotMarkedReadyByChannel = "SubscriptionNotMarkedReadyByChannel"
	channelReferenceFailed              = "ChannelReferenceFailed"
	subscriberResolveFailed             = "SubscriberResolveFailed"
	replyResolveFailed                  = "ReplyResolveFailed"
	deadLetterSinkResolveFailed         = "DeadLetterSinkResolveFailed"
)

var (
	v1ChannelGVK = v1.SchemeGroupVersion.WithKind("Channel")
)

type Reconciler struct {
	// DynamicClientSet allows us to configure pluggable Build objects
	dynamicClientSet dynamic.Interface

	// crdLister is used to resolve the ref version
	kreferenceResolver *kref.KReferenceResolver

	// listers index properties about resources
	subscriptionLister  listers.SubscriptionLister
	channelLister       listers.ChannelLister
	channelableTracker  eventingduck.ListableTracker
	destinationResolver *resolver.URIResolver
	tracker             tracker.Interface
}

// Check that our Reconciler implements Interface
var _ subscriptionreconciler.Interface = (*Reconciler)(nil)

// Check that our Reconciler implements Finalizer
var _ subscriptionreconciler.Finalizer = (*Reconciler)(nil)

// ReconcileKind implements Interface.ReconcileKind.
func (r *Reconciler) ReconcileKind(ctx context.Context, subscription *v1.Subscription) pkgreconciler.Event {
	// Find the channel for this subscription.
	channel, err := r.getChannel(ctx, subscription)
	if err != nil {
		logging.FromContext(ctx).Warnw("Failed to get Spec.Channel or backing channel as Channelable duck type",
			zap.Error(err),
			zap.Any("channel", subscription.Spec.Channel))
		subscription.Status.MarkReferencesResolvedUnknown(channelReferenceFailed, "Failed to get Spec.Channel or backing channel: %s", err)
		return pkgreconciler.NewEvent(corev1.EventTypeWarning, channelReferenceFailed, "Failed to get Spec.Channel or backing channel: %w", err)
	}

	// Make sure all the URI's that are suppose to be in status are up to date.
	if event := r.resolveSubscriptionURIs(ctx, subscription, channel); event != nil {
		return event
	}

	// Sync the resolved subscription into the channel.
	if event := r.syncChannel(ctx, channel, subscription); event != nil {
		return event
	}

	// No channel sync was needed.

	// Check if the channel has the subscription in its status.
	if event := r.checkChannelStatusForSubscription(ctx, channel, subscription); event != nil {
		return event
	}

	return nil
}

func (r *Reconciler) FinalizeKind(ctx context.Context, subscription *v1.Subscription) pkgreconciler.Event {
	channel, err := r.getChannel(ctx, subscription)
	if err != nil {
		// If the channel was deleted (i.e., error == notFound), just return nil so that
		// the subscription's finalizer is removed and the object is gc'ed.
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Remove the Subscription from the Channel's subscribers list only if it was actually added in the first place.
	if subscription.Status.IsAddedToChannel() {
		return r.syncChannel(ctx, channel, subscription)
	}
	return nil
}

func (r Reconciler) checkChannelStatusForSubscription(ctx context.Context, channel *eventingduckv1.Channelable, sub *v1.Subscription) pkgreconciler.Event {
	ss, err := r.getSubStatus(sub, channel)
	if err != nil {
		logging.FromContext(ctx).Warnw("Failed to get subscription status.", zap.Error(err))
		sub.Status.MarkChannelUnknown(subscriptionNotMarkedReadyByChannel, "Failed to get subscription status: %s", err)
		return pkgreconciler.NewEvent(corev1.EventTypeWarning, subscriptionNotMarkedReadyByChannel, "Failed to get subscription status: %w", err)
	}

	switch ss.Ready {
	case corev1.ConditionTrue:
		sub.Status.MarkChannelReady()
	case corev1.ConditionUnknown:
		sub.Status.MarkChannelUnknown(subscriptionNotMarkedReadyByChannel, "Subscription marked by Channel as Unknown")
	case corev1.ConditionFalse:
		sub.Status.MarkChannelFailed(subscriptionNotMarkedReadyByChannel, "Subscription marked by Channel as False")
	}

	return nil
}

func (r Reconciler) syncChannel(ctx context.Context, channel *eventingduckv1.Channelable, sub *v1.Subscription) pkgreconciler.Event {
	// Ok, now that we have the Channel and at least one of the Call/Result, let's reconcile
	// the Channel with this information.
	if patched, err := r.syncPhysicalChannel(ctx, sub, channel, false); err != nil {
		logging.FromContext(ctx).Warnw("Failed to sync physical Channel", zap.Error(err))
		sub.Status.MarkNotAddedToChannel(physicalChannelSyncFailed, "Failed to sync physical Channel: %v", err)
		return pkgreconciler.NewEvent(corev1.EventTypeWarning, physicalChannelSyncFailed, "Failed to synchronize to channel %q: %w", channel.Name, err)
	} else if patched {
		if sub.DeletionTimestamp.IsZero() {
			sub.Status.MarkAddedToChannel()
			return pkgreconciler.NewEvent(corev1.EventTypeNormal, "SubscriberSync", "Subscription was synchronized to channel %q", channel.Name)
		} else {
			return pkgreconciler.NewEvent(corev1.EventTypeNormal, "SubscriberRemoved", "Subscription was removed from channel %q", channel.Name)
		}
	}
	if sub.DeletionTimestamp.IsZero() {
		sub.Status.MarkAddedToChannel()
	}
	return nil
}

func (r *Reconciler) resolveSubscriptionURIs(ctx context.Context, subscription *v1.Subscription, channel *eventingduckv1.Channelable) pkgreconciler.Event {
	// Everything that was supposed to be resolved was, so flip the status bit on that.
	subscription.Status.MarkReferencesResolvedUnknown("Resolving", "Subscription resolution interrupted.")

	if err := r.resolveSubscriber(ctx, subscription); err != nil {
		return err
	}

	if err := r.resolveReply(ctx, subscription); err != nil {
		return err
	}

	if err := r.resolveDeadLetterSink(ctx, subscription, channel); err != nil {
		return err
	}

	// Everything that was supposed to be resolved was, so flip the status bit on that.
	subscription.Status.MarkReferencesResolved()
	return nil
}

func (r *Reconciler) resolveSubscriber(ctx context.Context, subscription *v1.Subscription) pkgreconciler.Event {
	// Resolve Subscriber.
	subscriber := subscription.Spec.Subscriber.DeepCopy()
	ctx = apis.WithinParent(ctx, subscription.ObjectMeta)

	if !isNilOrEmptyDestination(subscriber) {
		// This is done in the webhook too, but we need it here for backwards
		// compatibility for subscriptions with subscriber.ref.namespace = "".
		subscriber.SetDefaults(ctx)

		// Resolve the group
		if subscriber.Ref != nil && feature.FromContext(ctx).IsEnabled(feature.KReferenceGroup) {
			var err error
			subscriber.Ref, err = r.kreferenceResolver.ResolveGroup(subscriber.Ref)
			if err != nil {
				logging.FromContext(ctx).Warnw("Failed to resolve Subscriber.Ref",
					zap.Error(err),
					zap.Any("subscriber", subscriber))
				subscription.Status.MarkReferencesNotResolved(subscriberResolveFailed, "Failed to resolve spec.subscriber.ref: %v", err)
				return pkgreconciler.NewEvent(corev1.EventTypeWarning, subscriberResolveFailed, "Failed to resolve spec.subscriber.ref: %w", err)
			}
			logging.FromContext(ctx).Debugw("Group resolved", zap.Any("spec.subscriber.ref", subscriber.Ref))
		}

		subscriberURI, err := r.destinationResolver.URIFromDestinationV1(ctx, *subscriber, subscription)
		if err != nil {
			logging.FromContext(ctx).Warnw("Failed to resolve Subscriber",
				zap.Error(err),
				zap.Any("subscriber", subscriber))
			subscription.Status.MarkReferencesNotResolved(subscriberResolveFailed, "Failed to resolve spec.subscriber: %v", err)
			return pkgreconciler.NewEvent(corev1.EventTypeWarning, subscriberResolveFailed, "Failed to resolve spec.subscriber: %w", err)
		}
		// If there is a change in resolved URI, log it.
		if subscription.Status.PhysicalSubscription.SubscriberURI == nil || subscription.Status.PhysicalSubscription.SubscriberURI.String() != subscriberURI.String() {
			logging.FromContext(ctx).Debugw("Resolved Subscriber", zap.String("subscriberURI", subscriberURI.String()))
			subscription.Status.PhysicalSubscription.SubscriberURI = subscriberURI
		}
	} else {
		subscription.Status.PhysicalSubscription.SubscriberURI = nil
	}
	return nil
}

func (r *Reconciler) resolveReply(ctx context.Context, subscription *v1.Subscription) pkgreconciler.Event {
	// Resolve Reply.
	reply := subscription.Spec.Reply.DeepCopy()
	ctx = apis.WithinParent(ctx, subscription.ObjectMeta)

	if !isNilOrEmptyDestination(reply) {
		// This is done in the webhook too, but we need it here for backwards
		// compatibility for subscriptions with reply.ref.namespace = "".
		reply.SetDefaults(ctx)

		replyURI, err := r.destinationResolver.URIFromDestinationV1(ctx, *reply, subscription)
		if err != nil {
			logging.FromContext(ctx).Warnw("Failed to resolve reply",
				zap.Error(err),
				zap.Any("reply", reply))
			subscription.Status.MarkReferencesNotResolved(replyResolveFailed, "Failed to resolve spec.reply: %v", err)
			return pkgreconciler.NewEvent(corev1.EventTypeWarning, replyResolveFailed, "Failed to resolve spec.reply: %w", err)
		}
		// If there is a change in resolved URI, log it.
		if subscription.Status.PhysicalSubscription.ReplyURI == nil || subscription.Status.PhysicalSubscription.ReplyURI.String() != replyURI.String() {
			logging.FromContext(ctx).Debugw("Resolved reply", zap.String("replyURI", replyURI.String()))
			subscription.Status.PhysicalSubscription.ReplyURI = replyURI
		}
	} else {
		subscription.Status.PhysicalSubscription.ReplyURI = nil
	}
	return nil
}

func (r *Reconciler) resolveDeadLetterSink(ctx context.Context, subscription *v1.Subscription, channel *eventingduckv1.Channelable) pkgreconciler.Event {
	// resolve the Subscription's dls first, fall back to the Channels's
	if subscription.Spec.Delivery != nil && subscription.Spec.Delivery.DeadLetterSink != nil {
		deadLetterSinkURI, err := r.destinationResolver.URIFromDestinationV1(ctx, *subscription.Spec.Delivery.DeadLetterSink, subscription)
		if err != nil {
			subscription.Status.PhysicalSubscription.DeadLetterSinkURI = nil
			logging.FromContext(ctx).Warnw("Failed to resolve spec.delivery.deadLetterSink",
				zap.Error(err),
				zap.Any("delivery.deadLetterSink", subscription.Spec.Delivery.DeadLetterSink))
			subscription.Status.MarkReferencesNotResolved(deadLetterSinkResolveFailed, "Failed to resolve spec.delivery.deadLetterSink: %v", err)
			return pkgreconciler.NewEvent(corev1.EventTypeWarning, deadLetterSinkResolveFailed, "Failed to resolve spec.delivery.deadLetterSink: %w", err)
		}

		logging.FromContext(ctx).Debugw("Resolved deadLetterSink", zap.String("deadLetterSinkURI", deadLetterSinkURI.String()))
		subscription.Status.PhysicalSubscription.DeadLetterSinkURI = deadLetterSinkURI
		return nil
	}

	// In case there is no DLS defined in the Subscription Spec, fallback to Channel's
	if channel.Spec.Delivery != nil && channel.Spec.Delivery.DeadLetterSink != nil {
		if channel.Status.DeadLetterSinkURI != nil {
			logging.FromContext(ctx).Debugw("Resolved channel deadLetterSink", zap.String("deadLetterSinkURI", channel.Status.DeadLetterSinkURI.String()))
			subscription.Status.PhysicalSubscription.DeadLetterSinkURI = channel.Status.DeadLetterSinkURI
			return nil
		}
		subscription.Status.PhysicalSubscription.DeadLetterSinkURI = nil
		logging.FromContext(ctx).Warnw("Channel didn't set status.deadLetterSinkURI",
			zap.Any("delivery.deadLetterSink", channel.Spec.Delivery.DeadLetterSink))
		subscription.Status.MarkReferencesNotResolved(deadLetterSinkResolveFailed, "channel %s didn't set status.deadLetterSinkURI", channel.Name)
		return pkgreconciler.NewEvent(corev1.EventTypeWarning, deadLetterSinkResolveFailed, "channel %s didn't set status.deadLetterSinkURI", channel.Name)
	}

	// There is no DLS defined in neither Subscription nor the Channel
	subscription.Status.PhysicalSubscription.DeadLetterSinkURI = nil
	return nil
}

func (r *Reconciler) getSubStatus(subscription *v1.Subscription, channel *eventingduckv1.Channelable) (eventingduckv1.SubscriberStatus, error) {
	for _, sub := range channel.Status.Subscribers {
		if sub.UID == subscription.GetUID() &&
			sub.ObservedGeneration == subscription.GetGeneration() {
			return eventingduckv1.SubscriberStatus{
				UID:                sub.UID,
				ObservedGeneration: sub.ObservedGeneration,
				Ready:              sub.Ready,
				Message:            sub.Message,
			}, nil
		}
	}
	return eventingduckv1.SubscriberStatus{}, fmt.Errorf("subscription %q not present in channel %q subscriber's list", subscription.Name, channel.Name)
}

func (r *Reconciler) trackAndFetchChannel(ctx context.Context, sub *v1.Subscription, ref duckv1.KReference) (runtime.Object, pkgreconciler.Event) {
	// Resolve the group
	if feature.FromContext(ctx).IsEnabled(feature.KReferenceGroup) {
		newRef, err := r.kreferenceResolver.ResolveGroup(&ref)
		if err != nil {
			logging.FromContext(ctx).Warnw("Failed to resolve Channel reference",
				zap.Error(err),
				zap.Any("ref", ref))
			return nil, err
		}
		ref = *newRef
	}

	// Track the channel using the channelableTracker.
	// We don't need the explicitly set a channelInformer, as this will dynamically generate one for us.
	// This code needs to be called before checking the existence of the `channel`, in order to make sure the
	// subscription controller will reconcile upon a `channel` change.
	if err := r.channelableTracker.TrackInNamespaceKReference(ctx, sub)(ref); err != nil {
		return nil, pkgreconciler.NewEvent(corev1.EventTypeWarning, "TrackerFailed", "unable to track changes to spec.channel: %w", err)
	}
	chLister, err := r.channelableTracker.ListerForKReference(ref)
	if err != nil {
		logging.FromContext(ctx).Errorw("Error getting lister for Channel", zap.Any("channel", ref), zap.Error(err))
		return nil, err
	}
	obj, err := chLister.ByNamespace(sub.Namespace).Get(ref.Name)
	if err != nil {
		logging.FromContext(ctx).Errorw("Error getting channel from lister", zap.Any("channel", ref), zap.Error(err))
		return nil, err
	}
	return obj, err
}

// getChannel fetches the Channel as specified by the Subscriptions spec.Channel
// and verifies it's a channelable (so that we can operate on it via patches).
// If the Channel is a channels.messaging type (hence, it's only a factory for
// underlying channels), fetch and validate the "backing" channel.
func (r *Reconciler) getChannel(ctx context.Context, sub *v1.Subscription) (*eventingduckv1.Channelable, pkgreconciler.Event) {
	logging.FromContext(ctx).Infow("Getting channel", zap.Any("channel", sub.Spec.Channel))

	// 1. Track the channel pointed by subscription.
	//   a. If channel is a Channel.messaging.knative.dev
	obj, err := r.trackAndFetchChannel(ctx, sub, sub.Spec.Channel)
	if err != nil {
		logging.FromContext(ctx).Warnw("failed", zap.Any("channel", sub.Spec.Channel), zap.Error(err))
		return nil, err
	}

	gvk := obj.GetObjectKind().GroupVersionKind()

	// Test to see if the channel is Channel.messaging because it is going
	// to have a "backing" channel that is what we need to actually operate on
	// as well as keep track of.
	if v1ChannelGVK.Group == gvk.Group && v1ChannelGVK.Kind == gvk.Kind {
		// Track changes on Channel.
		// Ref: https://github.com/knative/eventing/issues/2641
		// NOTE: There is a race condition with using the channelableTracker
		// for Channel when mixed with the usage of channelLister. The
		// channelableTracker has a different cache than the channelLister,
		// when channelLister.Channels is called because the channelableTracker
		// caused an enqueue, the Channels cache my not have had time to
		// re-sync therefore we have to track Channels using a tracker linked
		// to the cache we intend to use to pull the Channel from. This linkage
		// is setup in NewController for r.tracker.
		if err := r.tracker.TrackReference(tracker.Reference{
			APIVersion: "messaging.knative.dev/v1",
			Kind:       "Channel",
			Namespace:  sub.Namespace,
			Name:       sub.Spec.Channel.Name,
		}, sub); err != nil {
			logging.FromContext(ctx).Infow("TrackReference for Channel failed", zap.Any("channel", sub.Spec.Channel), zap.Error(err))
			return nil, err
		}

		logging.FromContext(ctx).Debugw("fetching backing channel", zap.Any("channel", sub.Spec.Channel))
		// Because the above (trackAndFetchChannel) gives us back a Channelable
		// the status of it will not have the extra bits we need (namely, pointer
		// and status of the actual "backing" channel), we fetch it using typed
		// lister so that we get those bits.
		channel, err := r.channelLister.Channels(sub.Namespace).Get(sub.Spec.Channel.Name)
		if err != nil {
			return nil, err
		}

		if !channel.Status.IsReady() || channel.Status.Channel == nil {
			logging.FromContext(ctx).Warnw("backing channel not ready", zap.Any("channel", sub.Spec.Channel), zap.Any("backing channel", channel))
			return nil, fmt.Errorf("channel is not ready.")
		}

		statCh := duckv1.KReference{Name: channel.Status.Channel.Name, Namespace: sub.Namespace, Kind: channel.Status.Channel.Kind, APIVersion: channel.Status.Channel.APIVersion}
		obj, err = r.trackAndFetchChannel(ctx, sub, statCh)
		if err != nil {
			return nil, err
		}
	}

	// Now obj is supposed to be a Channelable, so check it.
	ch, ok := obj.(*eventingduckv1.Channelable)
	if !ok {
		logging.FromContext(ctx).Errorw("Failed to convert to Channelable Object", zap.Any("channel", sub.Spec.Channel), zap.Error(err))
		return nil, fmt.Errorf("Failed to convert to Channelable Object: %+v", obj)
	}

	return ch.DeepCopy(), nil
}

func isNilOrEmptyDestination(destination *duckv1.Destination) bool {
	return destination == nil || equality.Semantic.DeepEqual(destination, &duckv1.Destination{})
}

func (r *Reconciler) syncPhysicalChannel(ctx context.Context, sub *v1.Subscription, channel *eventingduckv1.Channelable, isDeleted bool) (bool, error) {
	logging.FromContext(ctx).Debugw("Reconciling physical from Channel", zap.Any("sub", sub))
	if patched, patchErr := r.patchSubscription(ctx, sub.Namespace, channel, sub); patchErr != nil {
		if isDeleted && apierrors.IsNotFound(patchErr) {
			logging.FromContext(ctx).Warnw("Could not find Channel", zap.Any("channel", sub.Spec.Channel))
			return false, nil
		}
		return patched, patchErr
	} else {
		return patched, nil
	}
}

func (r *Reconciler) patchSubscription(ctx context.Context, namespace string, channel *eventingduckv1.Channelable, sub *v1.Subscription) (bool, error) {
	after := channel.DeepCopy()

	if sub.DeletionTimestamp.IsZero() {
		r.updateChannelAddSubscription(after, sub)
	} else {
		r.updateChannelRemoveSubscription(after, sub)
	}

	patch, err := duck.CreateMergePatch(channel, after)
	if err != nil {
		return false, err
	}
	// If there is nothing to patch, we are good, just return.
	// Empty patch is {}, hence we check for that.
	if len(patch) <= 2 {
		return false, nil
	}

	resourceClient, err := eventingduck.ResourceInterface(r.dynamicClientSet, namespace, channel.GroupVersionKind())
	if err != nil {
		logging.FromContext(ctx).Warnw("Failed to create dynamic resource client", zap.Error(err))
		return false, err
	}
	patched, err := resourceClient.Patch(ctx, channel.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
	if err != nil {
		logging.FromContext(ctx).Warnw("Failed to patch the Channel", zap.Error(err), zap.Any("patch", patch))
		return false, err
	}
	logging.FromContext(ctx).Debugw("Patched resource", zap.Any("patch", patch), zap.Any("patched", patched))
	return true, nil
}

func (r *Reconciler) updateChannelRemoveSubscription(channel *eventingduckv1.Channelable, sub *v1.Subscription) {
	for i, v := range channel.Spec.Subscribers {
		if v.UID == sub.UID {
			channel.Spec.Subscribers = append(
				channel.Spec.Subscribers[:i],
				channel.Spec.Subscribers[i+1:]...)
			return
		}
	}
}

func (r *Reconciler) updateChannelAddSubscription(channel *eventingduckv1.Channelable, sub *v1.Subscription) {
	// Look to update subscriber.
	for i, v := range channel.Spec.Subscribers {
		if v.UID == sub.UID {
			channel.Spec.Subscribers[i].Generation = sub.Generation
			channel.Spec.Subscribers[i].SubscriberURI = sub.Status.PhysicalSubscription.SubscriberURI
			channel.Spec.Subscribers[i].ReplyURI = sub.Status.PhysicalSubscription.ReplyURI
			channel.Spec.Subscribers[i].Delivery = deliverySpec(sub, channel)
			return
		}
	}

	toAdd := eventingduckv1.SubscriberSpec{
		UID:           sub.UID,
		Generation:    sub.Generation,
		SubscriberURI: sub.Status.PhysicalSubscription.SubscriberURI,
		ReplyURI:      sub.Status.PhysicalSubscription.ReplyURI,
		Delivery:      deliverySpec(sub, channel),
	}

	// Must not have been found. Add it.
	channel.Spec.Subscribers = append(channel.Spec.Subscribers, toAdd)
}

func deliverySpec(sub *v1.Subscription, channel *eventingduckv1.Channelable) (delivery *eventingduckv1.DeliverySpec) {
	if sub.Spec.Delivery == nil && channel.Spec.Delivery != nil {
		// Default to the channel spec
		if sub.Status.PhysicalSubscription.DeadLetterSinkURI != nil {
			delivery = &eventingduckv1.DeliverySpec{
				DeadLetterSink: &duckv1.Destination{
					URI: sub.Status.PhysicalSubscription.DeadLetterSinkURI,
				},
			}
		}
		if channel.Spec.Delivery.BackoffDelay != nil ||
			channel.Spec.Delivery.Retry != nil ||
			channel.Spec.Delivery.BackoffPolicy != nil ||
			channel.Spec.Delivery.Timeout != nil ||
			channel.Spec.Delivery.RetryAfterMax != nil {
			if delivery == nil {
				delivery = &eventingduckv1.DeliverySpec{}
			}
			delivery.BackoffPolicy = channel.Spec.Delivery.BackoffPolicy
			delivery.Retry = channel.Spec.Delivery.Retry
			delivery.BackoffDelay = channel.Spec.Delivery.BackoffDelay
			delivery.Timeout = channel.Spec.Delivery.Timeout
			delivery.RetryAfterMax = channel.Spec.Delivery.RetryAfterMax
		}
		return
	}

	// Only set the deadletter sink if it's not nil. Otherwise we'll just end up patching
	// empty delivery in there.
	if sub.Status.PhysicalSubscription.DeadLetterSinkURI != nil {
		delivery = &eventingduckv1.DeliverySpec{
			DeadLetterSink: &duckv1.Destination{
				URI: sub.Status.PhysicalSubscription.DeadLetterSinkURI,
			},
		}
	}
	if sub.Spec.Delivery != nil &&
		(sub.Spec.Delivery.BackoffDelay != nil ||
			sub.Spec.Delivery.Retry != nil ||
			sub.Spec.Delivery.BackoffPolicy != nil ||
			sub.Spec.Delivery.Timeout != nil ||
			sub.Spec.Delivery.RetryAfterMax != nil) {
		if delivery == nil {
			delivery = &eventingduckv1.DeliverySpec{}
		}
		delivery.BackoffPolicy = sub.Spec.Delivery.BackoffPolicy
		delivery.Retry = sub.Spec.Delivery.Retry
		delivery.BackoffDelay = sub.Spec.Delivery.BackoffDelay
		delivery.Timeout = sub.Spec.Delivery.Timeout
		delivery.RetryAfterMax = sub.Spec.Delivery.RetryAfterMax
	}
	return
}
