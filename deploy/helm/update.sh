make generate-spec-yaml REGISTRY="GML_IMAGEREGISTRY" STAGINGVERSION="GML_VERSION" PROJECT="GML_PROJECT"

sed 's|GML_IMAGEREGISTRY|{{.Values.imageregistry}}|g' bin/gcs-fuse-csi-driver-specs-generated.yaml | \
  sed 's|GML_VERSION|{{.Values.version}}|g' | \
  sed 's|GML_PROJECT|{{.Values.project}}|g' > deploy/helm/templates/all.yaml
