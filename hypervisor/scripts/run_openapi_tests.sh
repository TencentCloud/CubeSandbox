#!/bin/bash
set -e
set -x

sudo docker run --rm -v ${PWD}:/local openapitools/openapi-generator-cli validate -i /local/vmm/src/api/openapi/cloud-hypervisor.yaml
