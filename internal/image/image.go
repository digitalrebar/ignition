package image

import (
	"fmt"
	"os/exec"

	"github.com/coreos/ignition/config/types"
	"github.com/coreos/ignition/internal/log"
)

func ApplyImage(logger *log.Logger, image types.Image, basePath string) error {
	logger.PushPrefix("applyImage")
	defer logger.PopPrefix()

	path := basePath
	if image.Path != nil {
		path = path + "/" + *image.Path
	}

	// Using source and type, get an image.  Run appropriate tool with path as dest.
	var middle, end string

        command := fmt.Sprintf("curl -sfL %s ", image.Source) 
	switch image.Type {
	case "dd-raw":
		middle = "| "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-tgz":
		middle = "| tar -xOzf - | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-txz":
		middle = "| tar -xOJf - | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-tbz":
		middle = "| tar -xOjf - | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-tar":
		middle = "| tar -xOf - | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-bz2":
		middle = "| bzcat | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-gz":
		middle = "| zcat | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "dd-xz":
		middle = "| xzcat | "
                end = fmt.Sprintf("dd of=%s bs=4M", path)
	case "tgz":
		middle = "| "
		end = fmt.Sprintf("tar -xzf - -C %s", path)
	case "wim":
		// XXX: This could be better alot better.
		middle = fmt.Sprintf("> %s/image.wim ; ", path)
		end = fmt.Sprintf("wimapply %s/image.wim 1 %s ; rm -f %s/image.wim", path, path, path)
	case "wim-pipe":
		middle = "| "
		end = fmt.Sprintf("wimapply - 1 %s", path)
	default:
		return fmt.Errorf("Unknown image type, %s, for %s", image.Type, image.Name)
	}

	cmd := command + middle + end
	if _, err := logger.LogCmd(exec.Command("bash", "-c", cmd), "copying image %q", image.Name); err != nil {
		return fmt.Errorf("image copy failed: %v", err)
	}

	return nil
}

