package api

import (
	"net/http"
	"nofx/fundsledger"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func fundsOptions(c *gin.Context, now time.Time) (fundsledger.Options, error) {
	days := 90
	q := c.Request.URL.Query()
	for key, values := range q {
		if (key == "days" || key == "tz" || key == "include_paper") && (len(values) != 1 || values[0] == "") {
			return fundsledger.Options{}, fundsParamError("funds parameters must have one non-empty value")
		}
	}
	if raw, ok := c.GetQuery("days"); ok {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 365 {
			return fundsledger.Options{}, fundsParamError("days must be an integer from 1 to 365")
		}
		days = n
	}
	tz := c.DefaultQuery("tz", "Asia/Shanghai")
	// LoadLocation accepts "Local", which is host-dependent, not an IANA zone.
	if tz == "" || tz == "Local" || strings.TrimSpace(tz) != tz {
		return fundsledger.Options{}, fundsParamError("tz must be a valid IANA timezone")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return fundsledger.Options{}, fundsParamError("tz must be a valid IANA timezone")
	}
	paper := c.DefaultQuery("include_paper", "0")
	if paper != "0" && paper != "1" {
		return fundsledger.Options{}, fundsParamError("include_paper must be 0 or 1")
	}
	return fundsledger.Options{Now: now, Days: days, Location: loc, IncludePaper: paper == "1"}, nil
}

type fundsParamError string

func (e fundsParamError) Error() string { return string(e) }

func (s *Server) handleFundsOverview(c *gin.Context) {
	userID := c.GetString("user_id")
	if userID == "" {
		SafeUnauthorized(c)
		return
	}
	o, err := fundsOptions(c, time.Now().UTC())
	if err != nil {
		SafeBadRequest(c, err.Error())
		return
	}
	traders, err := s.store.Trader().List(userID)
	if err != nil {
		SafeInternalError(c, "Load funds accounts", err)
		return
	}
	inputs := []fundsledger.AccountInput{}
	exchangeIDs := []string{}
	grouped := map[string]fundsSyncAccount{}
	for _, t := range traders {
		in, exchangeID, err := s.store.FundsAccount(userID, t.ID, o.Now)
		if err != nil {
			continue
		}
		if in.Kind == fundsledger.Paper && !o.IncludePaper {
			continue
		}
		inputs = append(inputs, in)
		exchangeIDs = append(exchangeIDs, exchangeID)
		if in.Kind == fundsledger.Paper {
			continue
		}
		// Recheck the owned configuration before using an exchange reader. Failure
		// is represented by warnings and cached data, never by loading credentials.
		kind, err := s.store.FundsKind(userID, t.ID)
		if err != nil || kind == fundsledger.Paper || exchangeID == "" {
			continue
		}
		group := grouped[exchangeID]
		if group.traders == nil {
			group = fundsSyncAccount{exchangeID: exchangeID, traders: map[string]string{}}
		}
		if group.traders[kind] == "" {
			group.traders[kind] = t.ID
		}
		grouped[exchangeID] = group
	}
	warnings := map[string]map[string][]string{}
	for id, group := range grouped {
		warnings[id] = s.syncFunds(c.Request.Context(), userID, group, o.Now)
	}
	for i := range inputs {
		in := &inputs[i]
		id := exchangeIDs[i]
		if in.Kind == fundsledger.Paper {
			continue
		}
		in.Warnings = append(in.Warnings, warnings[id][in.Kind]...)
		// Include records fetched by this request; DB reads remain wallet-scoped.
		income, err := s.store.AccountIncome().ListForKind(id, in.Kind, 0, o.Now.UnixMilli())
		if err == nil {
			in.Income = income
		} else {
			in.Warnings = append(in.Warnings, "收支流水读取失败，转账回退到残差推断")
		}
	}
	c.JSON(http.StatusOK, fundsledger.Aggregate(inputs, o))
}
