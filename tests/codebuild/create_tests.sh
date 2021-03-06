#!/bin/bash

source common.sh
source inject_tests.sh

# Applies each of the resources needed for the canary tests.
# Parameter:
#    $1: Namespace of the CRD
function run_canary_tests
{
  local crd_namespace="$1"

  echo "Injecting variables into tests"
  inject_all_variables

  echo "Starting Canary Tests"
  run_test "${crd_namespace}" testfiles/xgboost-mnist-trainingjob.yaml
  run_test "${crd_namespace}" testfiles/xgboost-mnist-hpo.yaml
  # Special code for batch transform till we fix issue-59
  run_test "${crd_namespace}" testfiles/xgboost-model.yaml
  # We need to get sagemaker model before running batch transform
  verify_test "${crd_namespace}" Model xgboost-model 1m Created
  yq w -i testfiles/xgboost-mnist-batchtransform.yaml "spec.modelName" "$(get_sagemaker_model_from_k8s_model "${crd_namespace}" xgboost-model)"
  run_test "${crd_namespace}" testfiles/xgboost-mnist-batchtransform.yaml 
  run_test "${crd_namespace}" testfiles/xgboost-hosting-deployment.yaml
  run_test "${crd_namespace}" testfiles/xgboost-mnist-trainingjob-debugger.yaml
}

# Applies each of the resources needed for the integration tests.
# Parameter:
#    $1: Namespace of the CRD
function run_integration_tests
{
  local crd_namespace="$1"
  run_canary_tests "${crd_namespace}"

  # TODO: Automate creation/testing of EFS file systems for relevant jobs
  # Build prerequisite resources
  if [ "$FSX_ID" == "" ]; then
    echo "Skipping build_fsx_from_s3 as fsx tests are disabled"
    #build_fsx_from_s3
  fi

  echo "Starting integration tests"
  run_test "${crd_namespace}" testfiles/spot-xgboost-mnist-trainingjob.yaml
  run_test "${crd_namespace}" testfiles/xgboost-mnist-custom-endpoint.yaml
  # run_test "${crd_namespace}" testfiles/efs-xgboost-mnist-trainingjob.yaml
  # run_test "${crd_namespace}" testfiles/fsx-kmeans-mnist-trainingjob.yaml
  run_test "${crd_namespace}" testfiles/spot-xgboost-mnist-hpo.yaml
  run_test "${crd_namespace}" testfiles/xgboost-mnist-hpo-custom-endpoint.yaml
  run_test "${crd_namespace}" testfiles/xgboost-mnist-trainingjob-debugger.yaml
}

# Verifies that all resources were created and are running/completed for the canary tests.
# Parameter:
#    $1: Namespace of the CRD
function verify_canary_tests
{
  local crd_namespace="$1"
  echo "Verifying canary tests"
  verify_test "${crd_namespace}" TrainingJob xgboost-mnist 20m Completed
  verify_test "${crd_namespace}" HyperparameterTuningJob xgboost-mnist-hpo 20m Completed
  verify_test "${crd_namespace}" BatchTransformJob xgboost-batch 20m Completed 
  verify_test "${crd_namespace}" HostingDeployment hosting 40m InService
  verify_test "${crd_namespace}" TrainingJob xgboost-mnist-debugger 20m Completed
}

# Verifies that all resources were created and are running/completed for the integration tests.
# Parameter:
#    $1: Namespace of the CRD
function verify_integration_tests
{
  local crd_namespace="$1"
  echo "Verifying integration tests"
  verify_canary_tests "${crd_namespace}"

  verify_test "${crd_namespace}" TrainingJob spot-xgboost-mnist 20m Completed
  verify_test "${crd_namespace}" TrainingJob xgboost-mnist-custom-endpoint 20m Completed
  # verify_test "${crd_namespace}" TrainingJob efs-xgboost-mnist 20m Completed
  # verify_test "${crd_namespace}" TrainingJob fsx-kmeans-mnist 20m Completed
  verify_test "${crd_namespace}" HyperparameterTuningJob spot-xgboost-mnist-hpo 20m Completed
  verify_test "${crd_namespace}" HyperparameterTuningJob xgboost-mnist-hpo-custom-endpoint 20m Completed
  verify_test "${crd_namespace}" TrainingJob xgboost-mnist-debugger 20m Completed
  # Verify that debug job has status
  verify_debug_test "${crd_namespace}" TrainingJob xgboost-mnist-debugger 20m NoIssuesFound
}

# This function verifies that a given debug job has specific status
# Parameter:
#    $1: CRD namespace
#    $1: CRD type
#    $2: Instance of CRD
#    $3: Single debug job status
function verify_debug_test
{
  local crd_namespace="$1"
  local crd_type="$2"
  local crd_instance="$3"
  local timeout="$4"
  local expected_debug_job_status="$5"
  # First verify that trainingjob has been completed
  verify_test "${crd_namespace}" TrainingJob xgboost-mnist-debugger $timeout Completed

  # TODO extend this for multiple debug job with debug job statuses parameter

  echo "Waiting for debug job to finish"
  timeout "${timeout}" bash -c \
    'until [ "$(kubectl get -n "$0" "$1" "$2" -o json | jq -r .status.debugRuleEvaluationStatuses[0].ruleEvaluationStatus)" == "$3" ]; do \
      echo "Debug job has not completed yet"; \
      sleep 1; \
    done' "${crd_namespace}" "${crd_type}" "${crd_instance}" "${expected_debug_job_status}"

  if [ $? -ne 0 ]; then
    echo "[FAILED] Debug job ${crd_type} ${crd_instance} with expected status ${expected_debug_job_status} has timed out"
    exit 1
  fi

  echo "Debug job has completed"
}

# This function verifies that job has started and not failed
# Parameter:
#    $1: Namespace of the CRD
#    $2: Kind of CRD
#    $3: Instance of CRD
#    $4: Timeout to complete the test
#    $5: The status that verifies the job has succeeded.
# e.g. verify_test default trainingjobs xgboost-mnist
function verify_test()
{
  local crd_namespace="$1"
  local crd_type="$2"
  local crd_instance="$3"
  local timeout="$4"
  local desired_status="$5"

  # Check if job exist
  kubectl get -n "${crd_namespace}" "${crd_type}" "${crd_instance}"
  if [ $? -ne 0 ]; then
    echo "[FAILED] ${crd_type} ${crd_namespace}:${crd_instance} job does not exist"
    exit 1
  fi

  # Wait until Training Job has started, there will be two rows, header(STATUS) and entry.
  # Entry will be none if the job has not started yet. When job starts none will disappear 
  # and real status will be present.
  echo "Waiting for ${crd_type} ${crd_namespace}:${crd_instance} job to start"
  timeout 1m bash -c \
    'until [ "$(kubectl get -n "$0" "$1" "$2" -o=custom-columns=STATUS:.status | grep -i none | wc -l)" -eq "0" ]; do \
      echo "${crd_type} ${crd_namespace}:${crd_instance} job has not started yet"; \
      sleep 1; \
    done' "${crd_namespace}" "${crd_type}" "${crd_instance}"

  # Job has started, check whether it has failed or not
  kubectl get -n "${crd_namespace}" "${crd_type}" "${crd_instance}" -o=custom-columns=NAME:.status | grep -i fail 
  if [ $? -eq 0 ]; then
    echo "[FAILED] ${crd_type} ${crd_namespace}:${crd_instance} job has failed" 
    exit 1
  fi

  echo "Waiting for ${crd_type} ${crd_namespace}:${crd_instance} job to complete"
  if ! wait_for_crd_status "$crd_namespace" "$crd_type" "$crd_instance" "$timeout" "$desired_status"; then
      echo "[FAILED] Waiting for status Completed failed"
      exit 1
  fi

  # Check weather job has completed or not
  echo "[PASSED] Verified ${crd_type} ${crd_namespace}:${crd_instance} has completed"
}

# Build a new FSX file system from an S3 data source to be used by FSX integration tests.
function build_fsx_from_s3()
{
  echo "Building FSX file system from S3 data source"

  NEW_FS="$(aws fsx create-file-system \
  --file-system-type LUSTRE \
  --lustre-configuration ImportPath=s3://${DATA_BUCKET}/kmeans_mnist_example \
  --storage-capacity 1200 \
  --subnet-ids subnet-187e9960 \
  --tags Key="Name",Value="$(date '+%Y-%m-%d-%H-%M-%S')" \
  --region us-west-2)"

  echo $NEW_FS

  FSX_ID="$(echo "$NEW_FS" | jq -r ".FileSystem.FileSystemId")"
  FS_AVAILABLE=CREATING
  until [[ "${FS_AVAILABLE}" != "CREATING" ]]; do
    FS_AVAILABLE="$(aws fsx --region us-west-2 describe-file-systems --file-system-id "${FSX_ID}" | jq -r ".FileSystems[0].Lifecycle")"
    sleep 30
  done
  aws fsx --region us-west-2 describe-file-systems --file-system-id ${FSX_ID}

  if [[ "${FS_AVAILABLE}" != "AVAILABLE" ]]; then
    exit 1
  fi

  export FSX_ID=$FSX_ID
}
