//go:build windows

package main

import "errors"

func targetFromState(_, _ string) (recoveryTarget, error) {
	return recoveryTarget{}, errors.New("su Windows usare --receipt e --archive; la lettura diretta del catalogo SQLite Linux non e disponibile")
}
