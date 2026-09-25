package api

import (
	"net/http"
	"nofx/config"
	"nofx/crypto"

	"github.com/gin-gonic/gin"
)

// CryptoHandler Encryption API handler
type CryptoHandler struct {
	cryptoService *crypto.CryptoService
}

// NewCryptoHandler Creates encryption handler
func NewCryptoHandler(cryptoService *crypto.CryptoService) *CryptoHandler {
	return &CryptoHandler{
		cryptoService: cryptoService,
	}
}

// ==================== Crypto Config Endpoint ====================

// HandleGetCryptoConfig Get crypto configuration
func (h *CryptoHandler) HandleGetCryptoConfig(c *gin.Context) {
	cfg := config.Get()
	c.JSON(http.StatusOK, gin.H{
		"transport_encryption": cfg.TransportEncryption,
	})
}

// ==================== Public Key Endpoint ====================

// HandleGetPublicKey Get server public key
func (h *CryptoHandler) HandleGetPublicKey(c *gin.Context) {
	cfg := config.Get()
	if !cfg.TransportEncryption {
		c.JSON(http.StatusOK, gin.H{
			"public_key":           "",
			"algorithm":            "",
			"transport_encryption": false,
		})
		return
	}

	publicKey := h.cryptoService.GetPublicKeyPEM()
	c.JSON(http.StatusOK, gin.H{
		"public_key":           publicKey,
		"algorithm":            "RSA-OAEP-2048",
		"transport_encryption": true,
	})
}

// (HandleDecryptSensitiveData removed 2026-09-25: a public generic decrypt
// oracle is a security regression of 1aea7abc — decryption belongs ONLY in
// the specific authenticated handlers that consume an encrypted payload.)

// ==================== Audit Log Query Endpoint ====================

// Audit log functionality removed, not needed in current simplified implementation

// ==================== Utility Functions ====================

// isValidPrivateKey Validate private key format
func isValidPrivateKey(key string) bool {
	// EVM private key: 64 hex characters (optional 0x prefix)
	if len(key) == 64 || (len(key) == 66 && key[:2] == "0x") {
		return true
	}
	// TODO: Add validation for other chains
	return false
}
