package migimagestream

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	imagev1API "github.com/openshift/api/image/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/fusor/openshift-migration-plugin/velero-plugins/migcommon"
	"github.com/fusor/openshift-velero-plugin/velero-plugins/common"
	v1 "github.com/heptio/velero/pkg/apis/velero/v1"
	"github.com/heptio/velero/pkg/plugin/velero"
)

// BackupPlugin is a backup item action plugin for Heptio Ark.
type BackupPlugin struct {
	Log logrus.FieldLogger
}

// AppliesTo returns a velero.ResourceSelector that applies to everything.
func (p *BackupPlugin) AppliesTo() (velero.ResourceSelector, error) {
	return velero.ResourceSelector{
		IncludedResources: []string{"imagestreams"},
	}, nil
}

// Execute copies local registry images into migration registry
func (p *BackupPlugin) Execute(item runtime.Unstructured, backup *v1.Backup) (runtime.Unstructured, []velero.ResourceIdentifier, error) {
	migrating := migcommon.IsMigBackup(backup)
	if !migrating {
		return item, nil, nil
	}

	p.Log.Info("[is-backup] Entering Imagestream backup plugin")

	im := imagev1API.ImageStream{}
	itemMarshal, _ := json.Marshal(item)
	json.Unmarshal(itemMarshal, &im)
	p.Log.Info(fmt.Sprintf("[is-backup] image: %#v", im))
	annotations := im.Annotations
	if annotations == nil {
		annotations = make(map[string]string)
	}

	internalRegistry := annotations[common.BackupRegistryHostname]
	if len(internalRegistry) == 0 {
		return nil, nil, errors.New("backup cluster registry not found for annotation \"openshift.io/backup-registry-hostname\"")
	}
	migrationRegistry := backup.Annotations[migcommon.MigrationRegistry]
	if len(migrationRegistry) == 0 {
		return nil, nil, errors.New("migration registry not found for annotation \"openshift.io/migration\"")
	}
	p.Log.Info(fmt.Sprintf("[is-backup] internal registry: %#v", internalRegistry))

	localImageCopied := false
	localImageCopiedByTag := false
	for _, tag := range im.Status.Tags {
		p.Log.Info(fmt.Sprintf("[is-backup] Backing up tag: %#v", tag.Tag))
		specTag := findSpecTag(im.Spec.Tags, tag.Tag)
		copyToTag := true
		if specTag != nil && specTag.From != nil {
			p.Log.Info(fmt.Sprintf("[is-backup] image tagged: %s, %s", specTag.From.Kind, specTag.From.Name))
			// we have a tag.
			copyToTag = false
		}
		// Iterate over items in reverse order so most recently tagged is copied last
		for i := len(tag.Items) - 1; i >= 0; i-- {
			dockerImageReference := tag.Items[i].DockerImageReference
			if strings.HasPrefix(dockerImageReference, internalRegistry) {
				localImageCopied = true
				destTag := ""
				if copyToTag {
					localImageCopiedByTag = true
					destTag = ":" + tag.Tag
				}
				srcPath := fmt.Sprintf("docker://%s", dockerImageReference)
				destPath := fmt.Sprintf("docker://%s/%s/%s%s", migrationRegistry, im.Namespace, im.Name, destTag)
				p.Log.Info(fmt.Sprintf("[is-backup] copying from: %s", srcPath))
				p.Log.Info(fmt.Sprintf("[is-backup] copying to: %s", destPath))

				manifest, err := copyImageBackup(srcPath, destPath)
				if err != nil {
					p.Log.Info(fmt.Sprintf("[is-backup] Error copying image: %v", err))
					return nil, nil, err
				}
				p.Log.Info(fmt.Sprintf("[is-backup] manifest of copied image: %s", manifest))
			}
		}
	}
	p.Log.Info(fmt.Sprintf("copied at least one local image: %t", localImageCopied))
	p.Log.Info(fmt.Sprintf("copied at least one local image by tag: %t", localImageCopiedByTag))

	im.Annotations = annotations

	var out map[string]interface{}
	objrec, _ := json.Marshal(im)
	json.Unmarshal(objrec, &out)
	item.SetUnstructuredContent(out)

	return item, nil, nil
}

func findStatusTag(tags []imagev1API.NamedTagEventList, name string) *imagev1API.NamedTagEventList {
	for _, tag := range tags {
		if tag.Tag == name {
			return &tag
		}
	}
	return nil
}

func copyImageBackup(src, dest string) (string, error) {
	sourceCtx, err := internalRegistrySystemContext()
	if err != nil {
		return "", err
	}
	destinationCtx, err := migrationRegistrySystemContext()
	if err != nil {
		return "", err
	}
	return copyImage(src, dest, sourceCtx, destinationCtx)
}
