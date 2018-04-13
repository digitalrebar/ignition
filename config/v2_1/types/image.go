package types

import (
	"errors"
	"fmt"

	"github.com/coreos/ignition/config/validate/report"
)

var (
	ErrImageInvalidType         = errors.New("image type is not valid")
)

func (i Image) Validate() report.Report {
	r := report.Report{}
	switch i.Type {
	case "dd-raw":
	case "dd-tgz":
	case "dd-txz":
	case "dd-tbz":
	case "dd-tar":
	case "dd-bz2":
	case "dd-gz":
	case "dd-xz":
	case "tgz":
	case "wim":
	case "wim-pipe":
	default:
		r.Add(report.Entry{
			Message: ErrImageInvalidType.Error(),
			Kind:    report.EntryError,
		})
	}
	return r
}

func (i Image) ValidateSource() report.Report {
	r := report.Report{}
        err := validateURL(i.Source)
        if err != nil {
                r.Add(report.Entry{
                        Message: fmt.Sprintf("invalid url %q: %v", i.Source, err),
                        Kind:    report.EntryError,
                })
        }
	return r
}
