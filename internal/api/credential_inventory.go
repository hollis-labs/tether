package api

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/hollis-labs/tether/internal/identity"
)

func credentialInventory(store *identity.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Reuse the existing administration gate; inventory is not issuance
		// authority and must remain closed even in local observe/off mode.
		if !identity.Verified(r.Context()) || !deviceAdmin(r) {
			writeError(w, http.StatusForbidden, CodeForbidden, "credential administration required")
			return
		}
		values, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid inventory query")
			return
		}
		q := identity.CredentialQuery{Kind: values.Get("kind"), PrincipalID: values.Get("principal")}
		for key, v := range values {
			if len(v) != 1 || (key != "kind" && key != "principal" && key != "after" && key != "limit") {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "unknown or repeated inventory parameter")
				return
			}
		}
		if values.Has("after") {
			q.After, err = strconv.ParseInt(values.Get("after"), 10, 64)
			if err != nil || q.After < 0 {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "after must be a nonnegative credential ID")
				return
			}
		}
		if values.Has("limit") {
			q.Limit, err = strconv.Atoi(values.Get("limit"))
			if err != nil || q.Limit < 1 {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, "limit must be between 1 and 200")
				return
			}
		}
		if err := q.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		page, err := store.ListCredentials(r.Context(), q)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, CodeInternalError, "credential inventory unavailable")
			return
		}
		writeJSON(w, http.StatusOK, page)
	}
}
