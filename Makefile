# Build/Release package configuration
release:
	cp controller/manifests/crd.yml config/crd.yml
	kctrl package release -y -v ${VERSION}
	cp carvel-artifacts/packages/argocd-attach.fling.vsphere.vmware.com/metadata.yml ./argo-attach.yml
	echo "\n---" >> ./argo-attach.yml
	cat carvel-artifacts/packages/argocd-attach.fling.vsphere.vmware.com/package.yml >> ./argo-attach.yml
build-controller:
	docker build -f controller/Dockerfile -t ghcr.io/warroyo/argocd-attach-service/controller:${VERSION} controller/.
release-controller:
	docker push ghcr.io/warroyo/argocd-attach-service/controller:${VERSION}