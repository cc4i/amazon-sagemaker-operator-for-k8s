/*
Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.

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

package trainingjob

import (
	"context"
	"errors"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	trainingjobv1 "go.amzn.com/sagemaker/sagemaker-k8s-operator/api/v1/trainingjob"
	. "go.amzn.com/sagemaker/sagemaker-k8s-operator/controllers"
	. "go.amzn.com/sagemaker/sagemaker-k8s-operator/controllers/sdkutil"

	aws "github.com/aws/aws-sdk-go-v2/aws"
	awserr "github.com/aws/aws-sdk-go-v2/aws/awserr"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker"
	"github.com/aws/aws-sdk-go-v2/service/sagemaker/sagemakeriface"
	"github.com/go-logr/logr"
)

// +kubebuilder:rbac:groups=sagemaker.aws.amazon.com,resources=trainingjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sagemaker.aws.amazon.com,resources=trainingjobs/status,verbs=get;update;patch

// TrainingJobReconciler reconciles a TrainingJob object
type TrainingJobReconciler struct {
	client.Client
	Log                   logr.Logger
	PollInterval          time.Duration
	createSageMakerClient SageMakerClientProvider
	awsConfigLoader       AwsConfigLoader
}

// Create a new reconciler with the default SageMaker client.
func NewTrainingJobReconciler(client client.Client, log logr.Logger, pollInterval time.Duration) *TrainingJobReconciler {
	return &TrainingJobReconciler{
		Client:       client,
		Log:          log,
		PollInterval: pollInterval,
		createSageMakerClient: func(cfg aws.Config) sagemakeriface.ClientAPI {
			return sagemaker.New(cfg)
		},
		awsConfigLoader: NewAwsConfigLoader(),
	}
}

func (r *TrainingJobReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	var ctx = context.Background()
	var state trainingjobv1.TrainingJob
	var log = r.Log.WithValues("trainingjob", req.NamespacedName)
	var cwLogUrl string

	log.Info("Getting resource")

	////////////////////////////////////////////////////////////////////////////////////////////////////
	// GET STATE FROM ETCD
	////////////////////////////////////////////////////////////////////////////////////////////////////

	if err := r.Get(ctx, req.NamespacedName, &state); err != nil {
		log.Info("Unable to fetch TrainingJob job", "reason", err)
		return RequeueIfError(IgnoreNotFound(err))
	}

	if state.Status.TrainingJobStatus == "" {
		status := InitializingJobStatus
		log.Info("Job status is empty, setting to intermediate status", "status", status)
		if err := r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			TrainingJobStatus: status,
			LastCheckTime:     Now(),
		}); err != nil {
			return RequeueIfError(err)
		}

		return RequeueImmediately()
	}

	// Generate the SageMaker training job name if user does not specifies in spec
	if state.Spec.TrainingJobName == nil || len(*state.Spec.TrainingJobName) == 0 {
		trainingJobName := getTrainingJobName(state)
		state.Spec.TrainingJobName = &trainingJobName

		log.Info("Adding generated name to spec", "new-name", trainingJobName)
		if err := r.Update(ctx, &state); err != nil {
			log.Info("Failed to add generated name to spec", "error", err)
			// Requeue as the update was not successful; this will guarantee another reconciler loop.
			return RequeueIfError(err)
		}

		// No requeue required because we generate an update by modifying the Spec.
		// If we return a requeue here, it will cause two concurrent reconciler loops because
		// the spec update generates a new reconcile call.
		// To avoid this, we return NoRequeue here and rely on the update generated by etcd.
		return NoRequeue()
	}

	log = log.WithValues("training-job-name", *state.Spec.TrainingJobName)

	var err error
	var sageMakerClient sagemakeriface.ClientAPI
	if cfg, err := r.awsConfigLoader.LoadAwsConfigWithOverrides(*state.Spec.Region, state.Spec.SageMakerEndpoint); err != nil {
		log.Error(err, "Error loading AWS config")
		return NoRequeue()
	} else {
		sageMakerClient = r.createSageMakerClient(cfg)
		log = log.WithValues("aws-region", cfg.Region)
		log.Info("Loaded AWS config")
	}

	//TODO: Convert it to tinyurl or even better can we expose CW url via API server proxy UI?
	cwLogUrl = "https://" + *state.Spec.Region + ".console.aws.amazon.com/cloudwatch/home?region=" +
		*state.Spec.Region + "#logStream:group=/aws/sagemaker/TrainingJobs;prefix=" +
		*state.Spec.TrainingJobName + ";streamFilter=typeLogStreamPrefix"

	describeRequest := sageMakerClient.DescribeTrainingJobRequest(&sagemaker.DescribeTrainingJobInput{
		TrainingJobName: aws.String(*state.Spec.TrainingJobName),
	})
	log.Info("Calling SM API DescribeTrainingJob")
	describeResponse, descErr := describeRequest.Send(ctx)
	awsErr, ok := descErr.(awserr.RequestFailure)

	// examine DeletionTimestamp to determine if object is under deletion
	if !state.ObjectMeta.DeletionTimestamp.IsZero() {
		if descErr == nil {
			// If it exist in sagemaker just delete it
			// If this job has finalizer the function will delete from sagemaker else it will just not requeue it
			return r.deleteTrainingJobIfFinalizerExists(ctx, log, state, sageMakerClient, describeResponse.DescribeTrainingJobOutput, cwLogUrl)
		} else {
			// It does not exist in sagemaker hence just remove the finalizer and update the state
			if ok {
				if r.isSageMaker404Response(awsErr) {
					log.Info("Training job does not exist in sagemaker, removing finalizer")
					return r.removeFinalizerAndUpdate(ctx, state, log)
				}
				// handle the 500 or unrecoverable API Error
				return r.handleSageMakerApiError(awsErr, ctx, log, state, cwLogUrl)
			}
		}
	}

	//////////////////////////////////////////////////////////////////////////////////////////////////////////
	//// HANDLE CREATION
	//////////////////////////////////////////////////////////////////////////////////////////////////////////
	// TODO Refactor this error checking. `descErr` can not be of type `RequestFailure` if, for example, the client is
	// given bad configuration.
	if ok {
		// If training job does not yet exist, we need to create it.
		if r.isSageMaker404Response(awsErr) {
			log.Info("Training job does not yet exist in SageMaker, going to create it")
			return r.createSageMakerTrainingJob(ctx, log, state, sageMakerClient, cwLogUrl)
		}
		// handle the 500 and unrecoverable API error
		return r.handleSageMakerApiError(awsErr, ctx, log, state, cwLogUrl)
	}

	trainingJobDescription := describeResponse.DescribeTrainingJobOutput

	////////////////////////////////////////////////////////////////////////////////////////////////////
	// VERIFY SPEC MATCHES DESCRIPTION
	////////////////////////////////////////////////////////////////////////////////////////////////////

	if comparison := TrainingJobSpecMatchesDescription(*trainingJobDescription, state.Spec); !comparison.Equal {
		log.Info("SageMaker job and Kubernetes spec differ. Updating status")
		const status = string(sagemaker.TrainingJobStatusFailed)
		err = r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			SageMakerTrainingJobName: *state.Spec.TrainingJobName,
			TrainingJobStatus:        status,
			Additional:               CreateSpecDiffersFromDescriptionErrorMessage(state, status, comparison.Differences),
			LastCheckTime:            Now(),
		})
		return RequeueIfError(err)
	}

	////////////////////////////////////////////////////////////////////////////////////////////////////
	// ADD FINALIZER IF NOT EXISTS
	////////////////////////////////////////////////////////////////////////////////////////////////////

	// examine DeletionTimestamp to determine if object is under deletion
	if state.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted. Add finalizer if not present
		if !ContainsString(state.ObjectMeta.Finalizers, SageMakerResourceFinalizerName) {
			state.ObjectMeta.Finalizers = append(state.ObjectMeta.Finalizers, SageMakerResourceFinalizerName)

			log.Info("Adding finalizer")

			prevGeneration := state.ObjectMeta.GetGeneration()
			if err = r.Update(ctx, &state); err != nil {
				return RequeueIfError(err)
			}
			return RequeueImmediatelyUnlessGenerationChanged(prevGeneration, state.ObjectMeta.GetGeneration())
		}
	}

	////////////////////////////////////////////////////////////////////////////////////////////////////
	// UPDATE ETCD TO MATCH SM API STATE
	////////////////////////////////////////////////////////////////////////////////////////////////////

	if !r.etcdMatchesSmApi(state, describeResponse) {
		if err = r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			SageMakerTrainingJobName: *state.Spec.TrainingJobName,
			TrainingJobStatus:        string(trainingJobDescription.TrainingJobStatus),
			SecondaryStatus:          string(trainingJobDescription.SecondaryStatus),
			LastCheckTime:            Now(),
			CloudWatchLogUrl:         cwLogUrl,
			Additional:               GetOrDefault(trainingJobDescription.FailureReason, ""),
		}); err != nil {
			log.Info("Error updating ETCD to sync with SM API state")
			return RequeueAfterInterval(r.PollInterval, err)
		}
		return RequeueAfterInterval(r.PollInterval, nil)
	}

	//////////////////////////////////////////////////////////////////////////////////////////////////////////
	// BASE CASES
	//////////////////////////////////////////////////////////////////////////////////////////////////////////

	switch trainingJobDescription.TrainingJobStatus {
	case sagemaker.TrainingJobStatusInProgress, sagemaker.TrainingJobStatusStopping:
		if err = r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			SageMakerTrainingJobName: *state.Spec.TrainingJobName,
			TrainingJobStatus:        string(trainingJobDescription.TrainingJobStatus),
			SecondaryStatus:          string(trainingJobDescription.SecondaryStatus),
			LastCheckTime:            Now(),
			CloudWatchLogUrl:         cwLogUrl,
		}); err != nil {
			log.Info("Error updating ETCD to sync with SM API state")
		}
		return RequeueAfterInterval(r.PollInterval, err)

	case sagemaker.TrainingJobStatusStopped, sagemaker.TrainingJobStatusFailed:
		return NoRequeue()

	case sagemaker.TrainingJobStatusCompleted:
		// If job has completed populate the model full path
		log.Info("Training has completed updating model path")
		// SageMaker stores the model artifact in OutputDataConfig path with path /output/model.tar.gz
		// SageMaker documentation https://docs.aws.amazon.com/sagemaker/latest/dg/cdf-training.html
		const outputPath string = "/output/model.tar.gz"
		modelPath := *state.Spec.OutputDataConfig.S3OutputPath + state.Status.SageMakerTrainingJobName + outputPath
		if err = r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			SageMakerTrainingJobName: *state.Spec.TrainingJobName,
			TrainingJobStatus:        string(trainingJobDescription.TrainingJobStatus),
			SecondaryStatus:          string(trainingJobDescription.SecondaryStatus),
			LastCheckTime:            Now(),
			CloudWatchLogUrl:         cwLogUrl,
			ModelPath:                modelPath,
		}); err != nil {
			log.Info("Error updating ETCD to sync with SM API state")
			return RequeueIfError(err)
		}
		return NoRequeue()

	default:
		unknownStateError := errors.New(string("Unknown Training Job Status " + trainingJobDescription.TrainingJobStatus))
		log.Error(unknownStateError, "Job is in unknown status")
		return NoRequeue()
	}
}

// Function to construct the sagemaker training job name
func getTrainingJobName(state trainingjobv1.TrainingJob) string {
	return GetGeneratedJobName(state.ObjectMeta.GetUID(), state.ObjectMeta.GetName(), 63)
}

func (r *TrainingJobReconciler) etcdMatchesSmApi(state trainingjobv1.TrainingJob, describeResponse *sagemaker.DescribeTrainingJobResponse) bool {
	primary_status_matches := state.Status.TrainingJobStatus == string(describeResponse.DescribeTrainingJobOutput.TrainingJobStatus)
	secondary_status_matches := state.Status.SecondaryStatus == string(describeResponse.DescribeTrainingJobOutput.SecondaryStatus)
	all_match := primary_status_matches && secondary_status_matches
	return all_match
}

func (r *TrainingJobReconciler) createSageMakerTrainingJob(ctx context.Context, log logr.Logger, state trainingjobv1.TrainingJob, sageMakerClient sagemakeriface.ClientAPI, cwUrl string) (ctrl.Result, error) {

	input := CreateCreateTrainingJobInputFromSpec(state.Spec)
	log.Info("Creating TrainingJob in SageMaker", "Request Parameters", input)

	createTrainingJobRequest := sageMakerClient.CreateTrainingJobRequest(&input)

	// Add `sagemaker-on-kubernetes` string literal to identify the k8s job in sagemaker
	aws.AddToUserAgent(createTrainingJobRequest.Request, SagemakerOnKubernetesUserAgentAddition)

	if _, err := createTrainingJobRequest.Send(ctx); err == nil {
		return RequeueImmediately()
	} else {

		awsErr, _ := err.(awserr.RequestFailure)
		// ok will be true, else we have sdk bug
		return r.handleSageMakerApiError(awsErr, ctx, log, state, cwUrl)
	}
}

func (r *TrainingJobReconciler) deleteTrainingJobIfFinalizerExists(ctx context.Context, log logr.Logger, state trainingjobv1.TrainingJob, sageMakerClient sagemakeriface.ClientAPI, trainingJobDescription *sagemaker.DescribeTrainingJobOutput, cwUrl string) (ctrl.Result, error) {
	log = log.WithName("deleteTrainingJobIfFinalizerExists")
	// The object is being deleted
	if ContainsString(state.ObjectMeta.Finalizers, SageMakerResourceFinalizerName) == false {
		log.Info("Object does not have finalizer nothing to do!!!")
		return NoRequeue()
	} else {
		log.Info("Object has been scheduled for deletion")
		switch trainingJobDescription.TrainingJobStatus {
		case sagemaker.TrainingJobStatusInProgress:
			log.WithName("Finalizer").Info("Job is in_progress, so we need to delete it")
			req := sageMakerClient.StopTrainingJobRequest(&sagemaker.StopTrainingJobInput{
				TrainingJobName: state.Spec.TrainingJobName,
			})
			_, err := req.Send(ctx)
			awsErr, ok := err.(awserr.RequestFailure)
			if ok {
				return r.handleSageMakerApiError(awsErr, ctx, log, state, cwUrl)
			}

			return RequeueImmediately()

		case sagemaker.TrainingJobStatusStopping:
			log.WithName("Finalizer").Info("Job is stopping, nothing to do")
			if err := r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
				SageMakerTrainingJobName: *state.Spec.TrainingJobName,
				TrainingJobStatus:        string(trainingJobDescription.TrainingJobStatus),
				SecondaryStatus:          string(trainingJobDescription.SecondaryStatus),
				LastCheckTime:            Now(),
				CloudWatchLogUrl:         cwUrl,
			}); err != nil {
				log.Info("Error updating ETCD to sync with SM API state")
			}
			return RequeueAfterInterval(r.PollInterval, nil)
		case sagemaker.TrainingJobStatusFailed, sagemaker.TrainingJobStatusCompleted, sagemaker.TrainingJobStatusStopped:
			log.WithName("Finalizer").Info("Job is in terminal state. Done")
			return r.removeFinalizerAndUpdate(ctx, state, log)
		default:
			unknownStateError := errors.New(string("Unknown Training Job Status " + trainingJobDescription.TrainingJobStatus))
			log.Error(unknownStateError, "Job is in unknown status")
			return NoRequeue()
		}
	}
}

// Remove the finalizer and update etcd
func (r *TrainingJobReconciler) removeFinalizerAndUpdate(ctx context.Context, state trainingjobv1.TrainingJob, log logr.Logger) (ctrl.Result, error) {
	log.Info("removeFinalizerAndUpdate")
	state.ObjectMeta.Finalizers = RemoveString(state.ObjectMeta.Finalizers, SageMakerResourceFinalizerName)

	err := r.Update(ctx, &state)
	return RequeueIfError(err)
}

// If this function returns an error, the status update has failed, and the reconciler should always requeue.
// This prevents the case where a terminal status fails to persist to the Kubernetes datastore yet we stop
// reconciling and thus leave the job in an unfinished state.
func (r *TrainingJobReconciler) updateJobStatus(ctx context.Context, log logr.Logger, trainingJob trainingjobv1.TrainingJob, source trainingjobv1.TrainingJobStatus) error {

	log = log.WithValues("new-status", source)
	log.Info("Updating job status")

	root := trainingJob.DeepCopy()
	// When you call this function, update/refresh all the fields since we overwrite.
	root.Status = source

	if err := r.Status().Update(ctx, root); err != nil {
		log.Error(err, "error updating job status")
		return err
	}

	return nil
}

// Retry on transient SM API error, fail permanently on other error
func (r *TrainingJobReconciler) handleSageMakerApiError(awsErr awserr.RequestFailure, ctx context.Context, log logr.Logger, state trainingjobv1.TrainingJob, cwLogUrl string) (ctrl.Result, error) {
	log = log.WithName("handleSageMakerApiError")

	if awsErr.StatusCode() >= 500 {
		log.Error(awsErr, "SageMaker server API error, will retry")
		return RequeueAfterInterval(r.PollInterval, awsErr)
	} else if r.isSageMaker429Response(awsErr) {
		log.Info("SageMaker rate limit exceeded, will retry", "err", awsErr)
		return RequeueAfterInterval(r.PollInterval, awsErr)
	} else {
		log.Error(awsErr, "Handling unrecoverable sagemaker API error")

		etcdUpdateErr := r.updateJobStatus(ctx, log, state, trainingjobv1.TrainingJobStatus{
			SageMakerTrainingJobName: *state.Spec.TrainingJobName,
			TrainingJobStatus:        string(sagemaker.TrainingJobStatusFailed),
			Additional:               awsErr.Error(),
			LastCheckTime:            Now(),
			CloudWatchLogUrl:         cwLogUrl,
		})

		return RequeueIfError(etcdUpdateErr)
	}
}

func (r *TrainingJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&trainingjobv1.TrainingJob{}).
		// Ignore status-only and metadata-only updates
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Complete(r)
}

// When we run describeTraining with the name of job which does not exist in sagemaker.
// SageMaker API treats this as a ValidationError, HTTP code 400. So the only way to
// disambiguate this from other errors is to check the message
func (r *TrainingJobReconciler) isSageMaker404Response(awsError awserr.RequestFailure) bool {
	return (awsError.Code() == "ValidationException") && (awsError.Message() == "Requested resource not found.")
}

// When we run describeTraining with the name of job, sagemaker returns throttling exception
// with error code 400 instead of 429.
func (r *TrainingJobReconciler) isSageMaker429Response(awsError awserr.RequestFailure) bool {
	return (awsError.Code() == "ThrottlingException") && (awsError.Message() == "Rate exceeded")
}