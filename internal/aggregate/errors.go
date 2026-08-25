package aggregate

import (
	"errors"

	"github.com/tfyl/mailroom/internal/mail"
)

func asProviderError(err error, target **mail.ProviderError) bool {
	return errors.As(err, target)
}
