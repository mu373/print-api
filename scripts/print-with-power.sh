#!/usr/bin/env bash
# Example workflow for a printer whose power is controlled through switch-api.
#
# It coordinates two independently running services:
#   1. Ask switch-api to run the logical switch's ON sequence.
#   2. Wait until print-api reports the configured CUPS printer as ready.
#   3. Submit one PDF and wait for its CUPS job to complete.
#   4. Ask switch-api to run the logical switch's OFF sequence.
#
# Safety policy: power is turned off only after a confirmed `completed` job.
# A timeout, an unavailable printer, or an uncertain/failed job leaves power on
# so that an operator can inspect the printer instead of interrupting a job.
#
# Required environment variables:
#   SWITCH_CONTROL_API_KEY  API key for switch-api.
#   PRINT_API_KEY           API key for print-api.
#
# Optional variables below select service URLs, logical switch/printer IDs,
# print preset, and timeouts. The default URLs match each service's default
# listen address; set them explicitly when either service uses another port.
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 /path/to/document.pdf" >&2
  exit 64
fi

document_path=$1
if [[ ! -f "$document_path" ]]; then
  echo "PDF does not exist: $document_path" >&2
  exit 66
fi

: "${SWITCH_CONTROL_API_KEY:?SWITCH_CONTROL_API_KEY must be set}"
: "${PRINT_API_KEY:?PRINT_API_KEY must be set}"

switch_api_url=${SWITCH_API_URL:-http://127.0.0.1:8010}
print_api_url=${PRINT_API_URL:-http://127.0.0.1:8000}
switch_id=${SWITCH_ID:-printer}
printer_id=${PRINT_PRINTER_ID:-brother-hl-l2460dw}
print_preset=${PRINT_PRESET:-letter-duplex-full}
poll_interval_seconds=${POLL_INTERVAL_SECONDS:-2}
startup_timeout_seconds=${STARTUP_TIMEOUT_SECONDS:-90}
job_timeout_seconds=${JOB_TIMEOUT_SECONDS:-300}

require_command() {
  command -v "$1" >/dev/null || {
    echo "required command is unavailable: $1" >&2
    exit 69
  }
}

require_command curl
require_command jq

switch_request() {
  curl --fail-with-body --silent --show-error --max-time 30 \
    -H "X-Api-Key: $SWITCH_CONTROL_API_KEY" \
    -X POST "$switch_api_url/switches/$switch_id/$1"
}

print_request() {
  curl --fail-with-body --silent --show-error --max-time 60 \
    -H "X-Api-Key: $PRINT_API_KEY" \
    -F "file=@${document_path};type=application/pdf" \
    -F "printer_id=$printer_id" \
    -F "preset=$print_preset" \
    "$print_api_url/print"
}

printer_status() {
  curl --fail-with-body --silent --show-error --max-time 15 \
    -H "X-Api-Key: $PRINT_API_KEY" \
    "$print_api_url/printers/$printer_id/status"
}

job_status() {
  curl --fail-with-body --silent --show-error --max-time 15 \
    -H "X-Api-Key: $PRINT_API_KEY" \
    "$print_api_url/jobs/$1"
}

echo "Powering on switch: $switch_id"
switch_request on >/dev/null

echo "Waiting for CUPS printer: $printer_id"
startup_deadline=$((SECONDS + startup_timeout_seconds))
while (( SECONDS < startup_deadline )); do
  current_printer_status=$(printer_status)
  current_printer_state=$(jq -r '.status' <<<"$current_printer_status")
  if [[ "$current_printer_state" == "ready" ]]; then
    break
  fi
  sleep "$poll_interval_seconds"
done

if [[ "${current_printer_state:-}" != "ready" ]]; then
  echo "printer did not become ready before timeout; leaving power on" >&2
  exit 75
fi

echo "Submitting print job"
submitted_job=$(print_request)
job_id=$(jq -er '.job_id' <<<"$submitted_job")
echo "Waiting for CUPS job: $job_id"

job_deadline=$((SECONDS + job_timeout_seconds))
while (( SECONDS < job_deadline )); do
  current_job_status=$(job_status "$job_id")
  current_job_state=$(jq -r '.status' <<<"$current_job_status")
  case "$current_job_state" in
    completed)
      echo "Job completed; powering off switch: $switch_id"
      switch_request off >/dev/null
      echo "Done"
      exit 0
      ;;
    queued|processing)
      sleep "$poll_interval_seconds"
      ;;
    canceled|aborted|unknown)
      echo "job status is $current_job_state; leaving power on" >&2
      exit 75
      ;;
    *)
      echo "unrecognized job status: $current_job_state; leaving power on" >&2
      exit 75
      ;;
  esac
done

echo "job did not complete before timeout; leaving power on" >&2
exit 75
