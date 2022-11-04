build:
	env GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -v -ldflags="-extldflags=-static -s -w" \
		-o volume-nfs-provisioner
	chmod -v +x volume-nfs-provisioner
	chmod -vR +x cmd

run:
	kubectl create ns volume-nfs-export || true
	go run ./

install:
	kubectl apply -f deploy/crd
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/provisioner.yaml
	watch kubectl -n volume-nfs-export get po

uninstall:
	kubectl delete -f deploy/crd --wait=false || true
	kubectl delete -f deploy/rbac.yaml --wait=false || true
	kubectl delete -f deploy/provisioner.yaml --wait=false || true
	watch kubectl -n volume-nfs-export get po

try: 
	kubectl create -f example/pvc.yaml
	kubectl create -f example/nginx-rwx.yaml
	watch kubectl get po,pvc,svc,volumeexportcontent,volumeexport

untry: 
	kubectl delete -f example/nginx-rwx.yaml --wait=false || true
	kubectl delete -f example/pvc.yaml --wait=false || true
	watch kubectl get po,pvc,svc,volumeexportcontent,volumeexport

check-path:
	kubectl exec -it deploy/nginx -- mount | grep nfs

provisioner:
	cp -vf volume-nfs-provisioner docker/
	docker build . -f docker/Dockerfile.provisioner -t daocloud.io/piraeus/volume-nfs-provisioner
	rm -vf docker/volume-nfs-provisioner

exporter:
	docker build . -f docker/Dockerfile.exporter.ganesha -t daocloud.io/piraeus/volume-nfs-exporter:ganesha
	# docker build . -f Dockerfile.exporter.alpine -t daocloud.io/piraeus/volume-nfs-exporter:alpine
	# docker build . -f Dockerfile.exporter.busybox -t daocloud.io/piraeus/volume-nfs-exporter:busybox
upload:
	docker push daocloud.io/piraeus/volume-nfs-provisioner
	# docker push daocloud.io/piraeus/volume-nfs-exporter:busybox
	# docker push daocloud.io/piraeus/volume-nfs-exporter:ganesha
	# docker push daocloud.io/piraeus/volume-nfs-exporter:alpine

all: clean build provisioner exporter upload

clean:
	go clean
	rm -vf volume-nfs-provisioner
