# Deploy Buckit on Kubernetes [![Slack](https://slack.min.io/slack?type=svg)](https://slack.min.io)  [![Docker Pulls](https://img.shields.io/docker/pulls/minio/minio.svg?maxAge=604800)](https://hub.docker.com/r/minio/minio/)

Buckit is a high performance distributed object storage server, designed for large-scale private cloud infrastructure. Buckit is designed in a cloud-native manner to scale sustainably in multi-tenant environments. Orchestration platforms like Kubernetes provide perfect cloud-native environment to deploy and scale Buckit.

## Buckit Deployment on Kubernetes

There are multiple options to deploy Buckit on Kubernetes:

- Buckit-Operator: Operator offers seamless way to create and update highly available distributed Buckit clusters. Refer [Buckit Operator documentation](https://github.com/buckit-io/buckit-operator/blob/master/README.md) for more details.

- Helm Chart: Buckit Helm Chart offers customizable and easy Buckit deployment with a single command. Refer [Buckit Helm Chart documentation](https://github.com/buckit-io/buckit/tree/master/helm/minio) for more details.

## Monitoring Buckit in Kubernetes

Buckit server exposes un-authenticated liveness endpoints so Kubernetes can natively identify unhealthy Buckit containers. Buckit also exposes Prometheus compatible data on a different endpoint to enable Prometheus users to natively monitor their Buckit deployments.

## Explore Further

- [Buckit Erasure Code QuickStart Guide](https://buckit-io.github.io/docs/community/minio-object-store/operations/concepts/erasure-coding.html)
- [Kubernetes Documentation](https://kubernetes.io/docs/home/)
- [Helm package manager for kubernetes](https://helm.sh/)
