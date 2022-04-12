# Kong Debug Tool

A tool for collecting debug information for Kong Gateway and Kong Mesh / Kuma.

## What is collected

The tool collects the following information and saves it to the local machine as a `.tar.gz` file:

- Logs for all Kong gateway instances (K8s / VM / Docker)
- Logs for all Kong Ingress Controller instances (K8s)
- Pod spec for all Kong gateway & ingress controller instances (K8s)
- Docker inspect information for all Kong gateway instances (Docker)
- kong.conf file (VM)
- Logs for all Kuma / Kong Mesh control-plane instances (K8s / Docker)
- Logs for all Kuma / Kong Mesh dataplanes (K8s / Docker)
- Pod spec for Kuma / Kong Mesh control-plane instances (K8s)
- Docker inspect information for all Kuma / Kong Mesh control-plane instances (Docker)
- Dataplane configuration files (VM)
- Dataplane (Envoy) logs (K8s / VM / Docker)
- Dataplane (Envoy) configuration dumps (K8s / VM / Docker)

## Privacy 

This tool _does not_ send any collected information outside of the machine where it was executed. It is the responsibility of the user executing this tool to ensure that sensitive information is not unintentionally collected _before_ they transmit the output file (s) to Kong. 

Examples of sensitive data that should be checked for include (but are not limited to):

- Keys
- Passwords
- Other secret data
- PII

## Security / Access

This tool runs with the privileges of the _executing user_ and does not elevate privileges at any time.