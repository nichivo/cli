package ca

import (
	"crypto/tls"
	"net/http"
	"os"
	"strings"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/api"
	"github.com/smallstep/certificates/ca"
	"github.com/smallstep/cli/command"
	"github.com/smallstep/cli/crypto/pki"
	"github.com/smallstep/cli/crypto/x509util"
	"github.com/smallstep/cli/errs"
	"github.com/smallstep/cli/flags"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/cli/ui"
	"github.com/urfave/cli"
)

/*
// RevocationReasonCodes is a map between string reason codes
// to integers as defined in RFC 5280
var RevocationReasonCodes = map[string]int{
	"unspecified":          ocsp.Unspecified,
	"keycompromise":        ocsp.KeyCompromise,
	"cacompromise":         ocsp.CACompromise,
	"affiliationchanged":   ocsp.AffiliationChanged,
	"superseded":           ocsp.Superseded,
	"cessationofoperation": ocsp.CessationOfOperation,
	"certificatehold":      ocsp.CertificateHold,
	"removefromcrl":        ocsp.RemoveFromCRL,
	"privilegewithdrawn":   ocsp.PrivilegeWithdrawn,
	"aacompromise":         ocsp.AACompromise,
}

// ReasonStringToCode tries to convert a reason string to an integer code
func ReasonStringToCode(reason string) (int, error) {
	// default to 0
	if reason == "" {
		return 0, nil
	}

	code, found := RevocationReasonCodes[strings.ToLower(reason)]
	if !found {
		return 0, errors.Errorf("unrecognized revocation reason '%s'", reason)
	}

	return code, nil
}
*/

func revokeCertificateCommand() cli.Command {
	return cli.Command{
		Name:   "revoke",
		Action: command.ActionFunc(revokeCertificateAction),
		Usage:  "revoke a certificate",
		UsageText: `**step ca revoke** <serial-number> <reason>
[**--crt**=<certificate>] [**--key**=<key>] [**--token**=<ott>]
[**--kid**=<key-id>] [**--ca-url**=<uri>] [**--root**=<file>]
[**--not-before**=<time|duration>] [**--not-after**=<time|duration>]
[**--reason**=<string>] [**-offline**]`,
		Description: `
**step ca revoke** command passively revokes a certificate with the given serial
number.

NOTE: This command currently only supports passive revocation. Passive revocation
means preventing a certificate from being renewed and letting it expire.

TODO: Add support for CRL and OCSP.

## POSITIONAL ARGUMENTS

<serial-number>
:  The serial number of the certificate that should be revoked.

## EXAMPLES

Revoke a certificate using a transparently generated token and the default reason:
'''
$ step ca revoke 308893286343609293989051180431574390766
'''

Revoke a certificate using a transparently generated token and configured reason:
'''
$ step ca revoke --reason "KeyCompromise" 308893286343609293989051180431574390766
'''

Revoke a certificate using that same certificate to setup an mTLS connection
with the CA:
'''
$ step ca revoke --crt mike.crt --key mike.key 308893286343609293989051180431574390766
'''

Revoke a certificate using a transparently generated token:
'''
$ step ca revoke "KeyCompromise"
'''

Revoke a certificate using a token, generated by a provisioner, to authorize
the request with the CA:
'''
$ step ca revoke --token <token> --reason "KeyCompromise" 308893286343609293989051180431574390766
'''`,
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "reason",
				Value: "",
				Usage: `The <reason> for which the certificate is being revoked.
If unset, default is Unspecified.

: <reason> is case-insensitive and must be one of:

    **Unspecified**

    **KeyCompromise**

    **CACompromise**

    **AffiliationChanged**

    **Superseded**

    **CessationOfOperation**

    **CertificateHold**

    **RemoveFromCRL**

    **PrivilegeWithdrawn**

    **AACompromise**
`,
			},
			cli.StringFlag{
				Name:  "crt",
				Usage: `The path to the <crt> that should be revoked.`,
			},
			cli.StringFlag{
				Name:  "key",
				Usage: `The path to the <key> corresponding to the cert that should be revoked.`,
			},
			tokenFlag,
			notBeforeFlag,
			notAfterFlag,
			caURLFlag,
			rootFlag,
			offlineFlag,
			caConfigFlag,
		},
	}
}

func revokeCertificateAction(ctx *cli.Context) error {
	err := errs.NumberOfArguments(ctx, 1)
	if err != nil {
		return err
	}

	args := ctx.Args()
	crtFile, keyFile := ctx.String("crt"), ctx.String("key")
	token := ctx.String("token")
	offline := ctx.Bool("offline")
	reason := ctx.String("reason")
	serial := args.Get(0)

	// offline and token are incompatible because the token is generated before
	// the start of the offline CA.
	if offline && len(token) != 0 {
		return errs.IncompatibleFlagWithFlag(ctx, "offline", "token")
	}

	// certificate flow unifies online and offline flows on a single api
	flow, err := newRevokeFlow(ctx, crtFile, keyFile)
	if err != nil {
		return err
	}

	if len(crtFile) > 0 || len(keyFile) > 0 {
		if len(crtFile) == 0 {
			return errs.RequiredWithFlag(ctx, "key", "crt")
		}
		if len(keyFile) == 0 {
			return errs.RequiredWithFlag(ctx, "crt", "key")
		}
		if len(token) > 0 {
		}
	} else if len(token) == 0 {
		// No token and no crt/key pair - so generate a token.
		token, err = flow.GenerateToken(ctx, serial)
		if err != nil {
			return err
		}
	}

	if err := flow.Revoke(ctx, serial, reason, token, crtFile, keyFile); err != nil {
		return err
	}

	ui.Printf("Certificate with Serial Number %s has been revoked.\n", serial)
	return nil
}

type revokeTokenClaims struct {
	SHA string `json:"sha"`
	jose.Claims
}

type revokeFlow struct {
	offlineCA *offlineCA
	offline   bool
}

func newRevokeFlow(ctx *cli.Context, crtFile, keyFile string) (*revokeFlow, error) {
	var err error
	var offlineClient *offlineCA

	offline := ctx.Bool("offline")
	if offline {
		caConfig := ctx.String("ca-config")
		if caConfig == "" {
			return nil, errs.InvalidFlagValue(ctx, "ca-config", "", "")
		}
		if len(crtFile) > 0 || len(keyFile) > 0 {
			offlineClient, err = newOfflineMTLSCA(caConfig, crtFile, keyFile)
		} else {
			offlineClient, err = newOfflineCA(caConfig)
		}
		if err != nil {
			return nil, err
		}
	}

	return &revokeFlow{
		offlineCA: offlineClient,
		offline:   offline,
	}, nil
}

func (f *revokeFlow) getClient(ctx *cli.Context, serial, token, crtFile, keyFile string) (caClient, error) {
	if f.offline {
		return f.offlineCA, nil
	}

	// Create online client
	rootFile := ctx.String("root")
	caURL := ctx.String("ca-url")

	if len(token) > 0 {
		tok, err := jose.ParseSigned(token)
		if err != nil {
			return nil, errors.Wrap(err, "error parsing flag '--token'")
		}
		var claims revokeTokenClaims
		if err := tok.UnsafeClaimsWithoutVerification(&claims); err != nil {
			return nil, errors.Wrap(err, "error parsing flag '--token'")
		}
		if strings.ToLower(claims.Subject) != strings.ToLower(serial) {
			return nil, errors.Errorf("token subject '%s' and certificate serial number '%s' do not match", claims.Subject, serial)
		}

		// Prepare client for bootstrap or provisioning tokens
		var options []ca.ClientOption
		if len(claims.SHA) > 0 && len(claims.Audience) > 0 && strings.HasPrefix(strings.ToLower(claims.Audience[0]), "http") {
			caURL = claims.Audience[0]
			options = append(options, ca.WithRootSHA256(claims.SHA))
		} else {
			if len(caURL) == 0 {
				return nil, errs.RequiredFlag(ctx, "ca-url")
			}
			if len(rootFile) == 0 {
				rootFile = pki.GetRootCAPath()
				if _, err := os.Stat(rootFile); err != nil {
					return nil, errs.RequiredFlag(ctx, "root")
				}
			}
			options = append(options, ca.WithRootFile(rootFile))
		}
		ui.PrintSelected("CA", caURL)
		return ca.NewClient(caURL, options...)
	}

	// If there is no token then we must be doing a Revoke over mTLS.
	cert, err := tls.LoadX509KeyPair(crtFile, keyFile)
	if err != nil {
		return nil, errors.Wrap(err, "error loading certificates")
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("error loading certificate: certificate chain is empty")
	}
	rootCAs, err := x509util.ReadCertPool(rootFile)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:                  rootCAs,
			PreferServerCipherSuites: true,
			Certificates:             []tls.Certificate{cert},
		},
	}

	ui.PrintSelected("CA", caURL)
	return ca.NewClient(caURL, ca.WithTransport(tr))
}

func (f *revokeFlow) GenerateToken(ctx *cli.Context, subject string) (string, error) {
	// For offline just generate the token
	if f.offline {
		return f.offlineCA.GenerateToken(ctx, revokeType, subject, nil)
	}

	// Use online CA to get the provisioners and generate the token
	caURL := ctx.String("ca-url")
	if len(caURL) == 0 {
		return "", errs.RequiredUnlessFlag(ctx, "ca-url", "token")
	}

	root := ctx.String("root")
	if len(root) == 0 {
		root = pki.GetRootCAPath()
		if _, err := os.Stat(root); err != nil {
			return "", errs.RequiredUnlessFlag(ctx, "root", "token")
		}
	}

	// parse times or durations
	notBefore, ok := flags.ParseTimeOrDuration(ctx.String("not-before"))
	if !ok {
		return "", errs.InvalidFlagValue(ctx, "not-before", ctx.String("not-before"), "")
	}
	notAfter, ok := flags.ParseTimeOrDuration(ctx.String("not-after"))
	if !ok {
		return "", errs.InvalidFlagValue(ctx, "not-after", ctx.String("not-after"), "")
	}

	var err error
	if subject == "" {
		subject, err = ui.Prompt("What DNS names or IP addresses would you like to use? (e.g. internal.smallstep.com)", ui.WithValidateNotEmpty())
		if err != nil {
			return "", err
		}
	}

	return newTokenFlow(ctx, revokeType, subject, nil, caURL, root, "", "", "", "", notBefore, notAfter)
}

func (f *revokeFlow) Revoke(ctx *cli.Context, serial, reason, token, crtFile, keyFile string) error {
	client, err := f.getClient(ctx, serial, token, crtFile, keyFile)
	if err != nil {
		return err
	}

	req := &api.RevokeRequest{
		Serial: serial,
		Reason: reason,
		OTT:    token,
	}

	if _, err = client.Revoke(req); err != nil {
		return err
	}
	return nil
}
