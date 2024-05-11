PLUGIN_NAME=jumager/ngcplogs
PLUGIN_TAG?=latest
PLUGIN_DIR=./ngcplogs-plugin
PLUGIN_SUPPORTED_ARCHS=linux/amd64 linux/arm64

all: clean build

clean:
	$(foreach ARCH,$(PLUGIN_SUPPORTED_ARCHS), $(call clean,${ARCH}))

build:
	$(foreach ARCH,$(PLUGIN_SUPPORTED_ARCHS), $(call build-plugin,${ARCH}))

build-all-cr: # For use in Github Action, so that GHCR plugin is also built, which is not necessary when building to install locally
	$(foreach ARCH,$(PLUGIN_SUPPORTED_ARCHS), $(call build-plugin,${ARCH},true))

push: clean build-all-cr
	$(foreach ARCH,$(PLUGIN_SUPPORTED_ARCHS), $(call push,${ARCH}))

define clean

ARCH=$(1)
$(eval TAG_ARCH=$(shell echo ${ARCH} | sed 's~/~-~g'))

@echo "### rm ${PLUGIN_DIR} for ${TAG_ARCH}"
sudo rm -rf ${PLUGIN_DIR}-${TAG_ARCH}

endef

define build-plugin
@echo

$(eval ARCH=$(shell echo $(1)))
$(eval BUILD_GHCR_PLUGIN=$(shell echo $(2)))
$(eval TAG_ARCH=$(shell echo ${ARCH} | sed 's~/~-~g'))

@echo "### docker build: rootfs image with ngcplogs"
docker buildx build --platform ${ARCH} -t ${PLUGIN_NAME}:${TAG_ARCH}-rootfs --load .

@echo "### create rootfs directory in ${PLUGIN_DIR}-${TAG_ARCH}/rootfs"
mkdir -p ${PLUGIN_DIR}-${TAG_ARCH}/rootfs
docker create --name tmprootfs --platform ${ARCH} ${PLUGIN_NAME}:${TAG_ARCH}-rootfs
docker export tmprootfs | tar -x -C ${PLUGIN_DIR}-${TAG_ARCH}/rootfs

@echo "### copy config.json to ${PLUGIN_DIR}-${TAG_ARCH}/"
cp config.json ${PLUGIN_DIR}-${TAG_ARCH}/
docker rm -vf tmprootfs

@echo "### remove existing plugin ${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} if exists"
docker plugin rm -f ${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} || true

@echo "### create new plugin ${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} from ${PLUGIN_DIR}-${TAG_ARCH}"
touch ${PLUGIN_DIR}-${TAG_ARCH}/rootfs/.dockerhub # Add a file so the generated plugin has a different hash than the ghcr version
docker plugin create ${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} ${PLUGIN_DIR}-${TAG_ARCH}
rm ${PLUGIN_DIR}-${TAG_ARCH}/rootfs/.dockerhub

$(if $(findstring ${BUILD_GHCR_PLUGIN},true), $(call build-ghcr-plugin,${ARCH}))

endef

define build-ghcr-plugin
@echo

ARCH=$(1)
$(eval TAG_ARCH=$(shell echo ${ARCH} | sed 's~/~-~g'))

@echo "### create new plugin ghcr.io/${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} from ${PLUGIN_DIR}-${TAG_ARCH}"
docker plugin rm -f ghcr.io/${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} || true
touch ${PLUGIN_DIR}-${TAG_ARCH}/rootfs/.ghcr # Add a file so the generated plugin has a different hash than the dockerhub version
docker plugin create ghcr.io/${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG} ${PLUGIN_DIR}-${TAG_ARCH}
rm ${PLUGIN_DIR}-${TAG_ARCH}/rootfs/.ghcr

endef

define push
@echo

ARCH=$(1)
$(eval TAG_ARCH=$(shell echo ${ARCH} | sed 's~/~-~g'))

docker plugin push ${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG}
#docker plugin push ghcr.io/${PLUGIN_NAME}:${TAG_ARCH}-${PLUGIN_TAG}

endef
