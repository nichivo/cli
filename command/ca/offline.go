package ca

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/pkg/errors"
	"github.com/smallstep/certificates/api"
	"github.com/smallstep/certificates/authority"
	"github.com/smallstep/cli/errs"
	"github.com/smallstep/cli/jose"
	"github.com/smallstep/cli/ui"
	"github.com/smallstep/cli/utils"
	"github.com/urfave/cli"
)

type offlineProvisionersSelect struct {
	Name         string
	Issuer       string
	Kid          string
	EncryptedKey string
}

type caClient interface {
	Sign(req *api.SignRequest) (*api.SignResponse, error)
	Renew(tr http.RoundTripper) (*api.SignResponse, error)
	Revoke(req *api.RevokeRequest, tr http.RoundTripper) (*api.RevokeResponse, error)
}

// offlineCA is a wrapper on top of the certificates authority methods that is
// used to sign certificates without an online CA.
type offlineCA struct {
	authority  *authority.Authority
	config     authority.Config
	configFile string
}

// newOfflineCA initializes an offliceCA.
func newOfflineCA(configFile string) (*offlineCA, error) {
	b, err := utils.ReadFile(configFile)
	if err != nil {
		return nil, err
	}

	var config authority.Config
	if err := json.Unmarshal(b, &config); err != nil {
		return nil, errors.Wrapf(err, "error reading %s", configFile)
	}

	if config.AuthorityConfig == nil || len(config.AuthorityConfig.Provisioners) == 0 {
		return nil, errors.Errorf("error parsing %s: no provisioners found", configFile)
	}

	auth, err := authority.New(&config)
	if err != nil {
		return nil, err
	}

	return &offlineCA{
		authority:  auth,
		config:     config,
		configFile: configFile,
	}, nil
}

// Audience returns the token audience.
func (c *offlineCA) Audience() string {
	return fmt.Sprintf("https://%s/sign", c.config.DNSNames[0])
}

// CaURL returns the CA URL using the first DNS entry.
func (c *offlineCA) CaURL() string {
	return fmt.Sprintf("https://%s", c.config.DNSNames[0])
}

// Root returns the path of the file used as root certificate.
func (c *offlineCA) Root() string {
	return c.config.Root.First()
}

// Provisioners returns the list of configured provisioners.
func (c *offlineCA) Provisioners() []*authority.Provisioner {
	return c.config.AuthorityConfig.Provisioners
}

// Sign is a wrapper on top of certificates Authorize and Sign methods. It
// returns an api.SignResponse with the requested certificate and the
// intermediate.
func (c *offlineCA) Sign(req *api.SignRequest) (*api.SignResponse, error) {
	opts, err := c.authority.Authorize(req.OTT)
	if err != nil {
		return nil, err
	}
	signOpts := authority.SignOptions{
		NotBefore: req.NotBefore,
		NotAfter:  req.NotAfter,
	}
	cert, ca, err := c.authority.Sign(req.CsrPEM.CertificateRequest, signOpts, opts...)
	if err != nil {
		return nil, err
	}
	return &api.SignResponse{
		ServerPEM:  api.Certificate{cert},
		CaPEM:      api.Certificate{ca},
		TLSOptions: c.authority.GetTLSOptions(),
	}, nil
}

// Renew is a wrapper on top of certificates Renew method. It returns an
// api.SignResponse with the requested certificate and the intermediate.
func (c *offlineCA) Renew(rt http.RoundTripper) (*api.SignResponse, error) {
	// it should not panic as this is always internal code
	tr := rt.(*http.Transport)
	asn1Data := tr.TLSClientConfig.Certificates[0].Certificate[0]
	peer, err := x509.ParseCertificate(asn1Data)
	if err != nil {
		return nil, errors.Wrap(err, "error parsing certificate")
	}
	// renew cert using authority
	cert, ca, err := c.authority.Renew(peer)
	if err != nil {
		return nil, err
	}
	return &api.SignResponse{
		ServerPEM:  api.Certificate{cert},
		CaPEM:      api.Certificate{ca},
		TLSOptions: c.authority.GetTLSOptions(),
	}, nil
}

// Revoke is a wrapper on top of certificates Revoke method. It returns an
// api.RevokeResponse.
func (c *offlineCA) Revoke(req *api.RevokeRequest, rt http.RoundTripper) (*api.RevokeResponse, error) {
	// Convert the reason to OCSP revocation code.
	reasonCode, err := api.ReasonStringToCode(req.Reason)
	if err != nil {
		return nil, err
	}

	// it should not panic as this is always internal code
	//tr := rt.(*http.Transport)
	//asn1Data := tr.TLSClientConfig.Certificates[0].Certificate[0]
	//peer, err := x509.ParseCertificate(asn1Data)
	//if err != nil {
	//	return nil, errors.Wrap(err, "error parsing certificate")
	//}
	// revoke cert using authority
	if err := c.authority.Revoke(req.Serial, "provisioner-id", reasonCode); err != nil {
		return nil, err
	}
	return &api.RevokeResponse{Status: "ok"}, nil
}

// GenerateToken creates the token used by the authority to sign certificates.
func (c *offlineCA) GenerateToken(ctx *cli.Context, typ, subject string, sans []string) (string, error) {
	// Use ca.json configuration for the root and audience
	root := c.Root()
	audience := c.Audience()

	// Get common parameters
	passwordFile := ctx.String("password-file")
	notBefore, notAfter, err := parseValidity(ctx)
	if err != nil {
		return "", err
	}

	// Get provisioner to use
	var kid, issuer, encryptedKey string
	provisioners := c.Provisioners()

	// Filter by kid (provisioner key id)
	if kid = ctx.String("kid"); len(kid) != 0 {
		provisioners = provisionerFilter(provisioners, func(p *authority.Provisioner) bool {
			return p.Key.KeyID == kid
		})
		if len(provisioners) == 0 {
			return "", errs.InvalidFlagValue(ctx, "kid", kid, "")
		}
	}

	// Filter by issuer (provisioner name)
	if issuer = ctx.String("issuer"); len(issuer) != 0 {
		provisioners = provisionerFilter(provisioners, func(p *authority.Provisioner) bool {
			return p.Name == issuer
		})
		if len(provisioners) == 0 {
			return "", errs.InvalidFlagValue(ctx, "issuer", issuer, "")
		}
	}

	if len(provisioners) == 1 {
		kid = provisioners[0].Key.KeyID
		issuer = provisioners[0].Name
		encryptedKey = provisioners[0].EncryptedKey
	} else {
		var items []*offlineProvisionersSelect
		for _, p := range provisioners {
			items = append(items, &offlineProvisionersSelect{
				Name:         p.Key.KeyID + " (" + p.Name + ")",
				Issuer:       p.Name,
				Kid:          p.Key.KeyID,
				EncryptedKey: p.EncryptedKey,
			})
		}
		i, _, err := ui.Select("What provisioner key do you want to use?", items, ui.WithSelectTemplates(ui.NamedSelectTemplates("Key ID")))
		if err != nil {
			return "", err
		}
		kid = items[i].Kid
		issuer = items[i].Issuer
		encryptedKey = items[i].EncryptedKey
	}

	// Decrypt encrypted key
	opts := []jose.Option{
		jose.WithUIOptions(ui.WithPromptTemplates(ui.PromptTemplates())),
	}
	if len(passwordFile) != 0 {
		opts = append(opts, jose.WithPasswordFile(passwordFile))
	}

	if len(encryptedKey) == 0 {
		return "", errors.Errorf("provisioner '%s' does not have an 'encryptedKey' property", kid)
	}

	decrypted, err := jose.Decrypt("Please enter the password to decrypt the provisioner key", []byte(encryptedKey), opts...)
	if err != nil {
		return "", err
	}

	jwk := new(jose.JSONWebKey)
	if err := json.Unmarshal(decrypted, jwk); err != nil {
		return "", errors.Wrap(err, "error unmarshalling provisioning key")
	}

	return generateToken(typ, subject, sans, kid, issuer, audience, root, notBefore, notAfter, jwk)
}
