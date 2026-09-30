package api

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/gin-gonic/gin"
	"net/http"
)

// Private keys never leave the browser. This endpoint validates public addresses only.
func (s *Server) handleWalletValidate(c *gin.Context) {
	var req struct {
		Address    string `json:"address"`
		PrivateKey string `json:"private_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.PrivateKey != "" {
		c.JSON(http.StatusBadRequest, gin.H{"valid": false, "error": "send a public address, never a private key"})
		return
	}
	if !common.IsHexAddress(req.Address) {
		c.JSON(http.StatusBadRequest, gin.H{"valid": false, "error": "invalid public address"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"valid": true, "address": common.HexToAddress(req.Address).Hex()})
}

func (s *Server) handleWalletGenerate(c *gin.Context) {
	c.JSON(http.StatusGone, gin.H{"error": "generate wallets locally; server-side private key generation is disabled"})
}
