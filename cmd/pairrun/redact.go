package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func runRedact(args []string) error {
	fs := flag.NewFlagSet("redact", flag.ContinueOnError)
	in := fs.String("in", "", "results directory")
	out := fs.String("out", "", "directory to create for the masked copy; it must not exist")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("pairrun redact: unexpected argument %q, want flags only", fs.Arg(0))
	}
	if *in == "" || *out == "" {
		return fmt.Errorf("pairrun redact: -in and -out are required\n%s", usage)
	}
	return redact(*in, *out)
}

// redact copies the results directory in to out, which must not exist, with
// the IPv4 addresses in the files masked (redactAddresses). The results are
// published, and they quote the address at which the client reached the
// engine: in the arguments of each run, in the plan, in loadgen's output and in
// the errors that the notes and problems repeat. On a rented Pod that is a
// public address. The files in stay as they are; only the copy is published.
//
// Every file is masked as bytes, whatever its encoding, so that none passes
// for not being text. An address that a file spells with a NUL byte between its
// characters, as UTF-16 does, is not found. An address is no JSON number, so in
// a JSON file it stands in a string and the file stays valid JSON. Files keep
// their permission bits. Anything but a regular file or a directory is an error
// that names it.
//
// A file that cannot be read or written, or whose masked bytes still hold an
// address that the mask does not leave, ends the copy with an error that names
// it, and out is removed: it holds all of the masked results or none.
func redact(in, out string) error {
	return redactWith(in, out, redactAddresses)
}

// redactWith is redact with the mask given, so that a test can make it leak.
func redactWith(in, out string, mask func([]byte) []byte) error {
	if fi, err := os.Stat(in); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", in)
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		return err
	}
	err := filepath.WalkDir(in, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(in, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(out, rel)
		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return os.Mkdir(dst, 0o755)
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", path)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data = mask(data)
		if leavesAddress(data) {
			return fmt.Errorf("%s: an address is left after masking", path)
		}
		return writeNewPerm(dst, data, info.Mode().Perm())
	})
	if err != nil {
		return errors.Join(err, os.RemoveAll(out))
	}
	return nil
}

// writeNewPerm writes data to a file that must not exist yet, with exactly the
// permission bits perm, which the process's umask would otherwise clear.
func writeNewPerm(name string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	return errors.Join(err, f.Chmod(perm), f.Close())
}

// redactedAddress is what redactAddresses puts in place of an address.
const redactedAddress = "x.x.x.x"

// redactAddresses returns text with every IPv4 address, and the port that
// follows it if there is one, replaced by redactedAddress, except 0.0.0.0 and
// 127.0.0.1 (the engine's own listen address and loopback), which stay with
// their port. An address is four decimal numbers of at most 255 joined by
// dots, with no digit before or after it and no dot and digit before or after
// it: a timestamp, a longer number or 1.2.3.4.5 is not one, but a version
// number of four parts is. IPv6 addresses are not redacted. The result is never
// longer than text.
func redactAddresses(text []byte) []byte {
	out := make([]byte, 0, len(text))
	for i := 0; i < len(text); {
		n := dottedQuad(text, i)
		if n == 0 {
			out = append(out, text[i])
			i++
			continue
		}
		quad := string(text[i : i+n])
		i += n
		if exempt(quad) {
			out = append(out, quad...)
			continue
		}
		out = append(out, redactedAddress...)
		i += portLen(text[i:])
	}
	return out
}

// exempt reports whether the mask leaves the address quad as it is.
func exempt(quad string) bool {
	return quad == "0.0.0.0" || quad == "127.0.0.1"
}

// leavesAddress reports whether text holds an address that the mask does not
// exempt, which a masked text must not.
func leavesAddress(text []byte) bool {
	for i := range text {
		if n := dottedQuad(text, i); n > 0 && !exempt(string(text[i:i+n])) {
			return true
		}
	}
	return false
}

// dottedQuad returns the length of the IPv4 address that starts at text[i],
// or 0 if none does.
func dottedQuad(text []byte, i int) int {
	digit := func(j int) bool { return j >= 0 && j < len(text) && text[j] >= '0' && text[j] <= '9' }
	if !digit(i) || digit(i-1) || i >= 1 && text[i-1] == '.' && digit(i-2) {
		return 0
	}
	j := i
	for octet := range 4 {
		if octet > 0 {
			if j >= len(text) || text[j] != '.' {
				return 0
			}
			j++
		}
		start, value := j, 0
		for ; digit(j) && j-start < 4; j++ {
			value = value*10 + int(text[j]-'0')
		}
		if j == start || j-start > 3 || value > 255 {
			return 0
		}
	}
	if j < len(text) && text[j] == '.' && digit(j+1) {
		return 0
	}
	return j - i
}

// portLen returns the length of the ":port" that starts text, or 0.
func portLen(text []byte) int {
	if len(text) == 0 || text[0] != ':' {
		return 0
	}
	n := 0
	for 1+n < len(text) && text[1+n] >= '0' && text[1+n] <= '9' {
		n++
	}
	if n == 0 || n > 5 {
		return 0
	}
	return 1 + n
}
