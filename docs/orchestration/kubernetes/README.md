# Deploy BuckIt on Kubernetes [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io)  [![Docker Pulls](https://img.shields.io/docker/pulls/minio/minio.svg?maxAge=604800)](https://hub.docker.com/r/minio/minio/)

BuckIt is a high performance distributed object storage server, designed for large-scale private cloud infrastructure. BuckIt is designed in a cloud-native manner to scale sustainably in multi-tenant environments. Orchestration platforms like Kubernetes provide perfect cloud-native environment to deploy and scale BuckIt.

## BuckIt Deployment on Kubernetes

There are multiple options to deploy BuckIt on Kubernetes:

- BuckIt-Operator: Operator offers seamless way to create and update highly available distributed BuckIt clusters. Refer [BuckIt Operator documentation](https://github.com/minio/minio-operator/blob/master/README.md) for more details.

- Helm Chart: BuckIt Helm Chart offers customizable and easy BuckIt deployment with a single command. Refer [BuckIt Helm Chart documentation](https://github.com/minio/minio/tree/master/helm/minio) for more details.

## Monitoring BuckIt in Kubernetes

BuckIt server exposes un-authenticated liveness endpoints so Kubernetes can natively identify unhealthy BuckIt containers. BuckIt also exposes Prometheus compatible data on a different endpoint to enable Prometheus users to natively monitor their BuckIt deployments.

## Explore Further

- [BuckIt Erasure Code QuickStart Guide](https://docs.min.io/community/minio-object-store/operations/concepts/erasure-coding.html)
- [Kubernetes Documentation](https://kubernetes.io/docs/home/)
- [Helm package manager for kubernetes](https://helm.sh/)
