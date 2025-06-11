# Build/Release package configuration
release:
	kctrl package release -y -v ${VERSION}
	cp carvel-artifacts/packages/argocd-attach.fling.vsphere.vmware.com/metadata.yml ./argo-attach.yml
	echo "\n---" >> ./argo-attach.yml
	cat carvel-artifacts/packages/argocd-attach.fling.vsphere.vmware.com/package.yml >> ./argo-attach.yml