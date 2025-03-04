package openhorizon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/vault/sdk/logical"
)

// Attempt to authenticate the caller as an open horizon user.
func (o *backend) AuthenticateAsUser(exURL, tok, userOrg, userId, password string) (*logical.Response, error) {

	// Verify that the caller's credentials are valid openhorizon exchange credentials.
	users, err := o.verifyUserCredentials(exURL, userOrg, userId, password)

	if _, ok := err.(NotAuthenticatedError); ok {
		o.Logger().Info(ohlog(fmt.Sprintf("(%s/%s) is not authenticated as a user: %v", userOrg, userId, err)))
		return nil, err
	} else if _, ok := err.(OtherError); ok {
		o.Logger().Error(ohlog(fmt.Sprintf("error trying to authenticate (%v/%v) as a user, error: %v", userOrg, userId, err)))
		return nil, err
	}

	// Log a successful exchange authentication
	if o.Logger().IsInfo() {
		o.Logger().Info(ohlog(fmt.Sprintf("user (%s/%s) successfuly authenticated with the exchange", userOrg, userId)))
	}

	// Attach ACL policies to the user's token to ensure that the user can only access secrets in it's org and secrets
	// that are private to that user. The policy that is attached is different for org admins vs non-admin users. Org admins
	// can work with org wide secrets and delete user private secrets.
	foundUser := false
	foundAdminUser := false
	policyName := ""
	var fullOrgUser string

	// Iterate through the users in the response. There should be one or none.
	for key, userInfo := range users.Users {

		// Ensure that the returned key is in the expected {orgid}/{username} format.
		orgAndUsername := strings.Split(key, "/")
		if len(orgAndUsername) != 2 {
			o.Logger().Error(ohlog(fmt.Sprintf("returned user (%s) has unsupported format, should be org/user", key)))
			return nil, logical.ErrPermissionDenied
		}

		// Log a found user
		fullOrgUser = key
		if o.Logger().IsInfo() {
			o.Logger().Info(ohlog(fmt.Sprintf("user (%s) found in the exchange", fullOrgUser)))
		}

		// Interrogate the response to find the user that we're trying to authenticate.
		if fullOrgUser == fmt.Sprintf("%s/%s", EX_ROOT_USER, EX_ROOT_USER) {
			// exchange root user (root/root:{pwd}), no permission
			o.Logger().Error(ohlog(fmt.Sprintf("user (root/root) is not supported")))
			return nil, logical.ErrPermissionDenied
		}

		if userInfo.HubAdmin {
			// hubAdmin, no permission
			o.Logger().Error(ohlog(fmt.Sprintf("user (%s) is a hubadmin, which is not supported", fullOrgUser)))
			return nil, logical.ErrPermissionDenied
		}

		if userInfo.Admin {

			// The user is an org admin.
			foundAdminUser = true

			// Ensure that the vault ACL policies needed by this user are defined in the vault.
			policyName, err = o.setupUserPolicies(userOrg, orgAndUsername[1], foundAdminUser, tok)
			if err != nil {
				o.Logger().Error(ohlog(fmt.Sprintf("unable to setup ACL policies for user (%s) as an admin, error: %v", fullOrgUser, err)))
				return nil, logical.ErrPermissionDenied
			}

		} else {

			// The user is a regular user.
			foundUser = true

			// Ensure that the vault ACL policies needed by this user are defined in the vault.
			policyName, err = o.setupUserPolicies(userOrg, orgAndUsername[1], !foundUser, tok)
			if err != nil {
				o.Logger().Error(ohlog(fmt.Sprintf("unable to setup ACL policies for user (%s), error: %v", fullOrgUser, err)))
				return nil, logical.ErrPermissionDenied
			}

		}

	}

	// The authenticated user was not found in the exchange, log the error and terminate the login.
	if !foundUser && !foundAdminUser {
		o.Logger().Error(ohlog(fmt.Sprintf("user (%s/%s) was not found in the exchange", userOrg, userId)))
		return nil, logical.ErrPermissionDenied
	}

	// Log a successful authentication.
	if o.Logger().IsInfo() {
		o.Logger().Info(ohlog(fmt.Sprintf("user (%s) authenticated", fullOrgUser)))
	}

	// Return the authentication results to the framework.
	// This response indicates a couple of things:
	// - The access control policies that should be given to the user.
	// - An indication if the user is an exchange admin, which conditions the ACLs that are applied.
	// - The time limit for which the token should remain valid, it can be used for a short time and then it expires.
	// - How the current lease for the token shuld behave, in this case it is not renewable.
	return &logical.Response{
		Auth: &logical.Auth{
			Policies: []string{policyName},
			Metadata: map[string]string{
				"admin":        strconv.FormatBool(foundAdminUser),
				"exchangeUser": fullOrgUser,
			},
			LeaseOptions: logical.LeaseOptions{
				TTL:       2 * time.Minute,
				Renewable: false,
			},
		},
	}, nil
}

// Call the openhorizon exchange to validate the caller's credentials. This API call will use the caller's own credentials to verify that it can
// retrieve the definition of it's own idenity from the exchange. This verifies that the caller's creds are good.
func (o *backend) verifyUserCredentials(exURL string, userOrg string, userId string, password string) (*GetUsersResponse, error) {

	// Log the exchange API that we are going to call.
	url := fmt.Sprintf("%v/orgs/%v/users/%v", exURL, userOrg, userId)
	user := fmt.Sprintf("%s/%s", userOrg, userId)
	apiMsg := fmt.Sprintf("%v %v", http.MethodGet, url)
	if o.Logger().IsDebug() {
		o.Logger().Debug(ohlog(fmt.Sprintf("checking exchange API %v", apiMsg)))
	}

	// Invoke the exchange API to verify the user.
	resp, err := o.InvokeExchangeWithRetry(url, user, password)

	// If there was an error invoking the HTTP API, return it.
	if err != nil {
		return nil, OtherError{Msg: err.Error()}
	}

	// Make sure the response reader is closed if we exit quickly.
	defer resp.Body.Close()

	// If the response code was not expected, then return the error.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == 401 {
			return nil, NotAuthenticatedError{Msg: fmt.Sprintf("unable to verify user (%v) in the exchange, HTTP code %v, either the user is undefined or the user's password is incorrect.", user, resp.StatusCode)}
		} else if resp.StatusCode == 404 {
			return nil, NotAuthenticatedError{Msg: fmt.Sprintf("user (%v/%v) not found in the exchange, HTTP code %v", userOrg, user, resp.StatusCode)}
		} else {
			return nil, OtherError{Msg: fmt.Sprintf("unable to verify user (%v) in the exchange, HTTP code %v", user, resp.StatusCode)}
		}
	}

	// Demarshal the response.
	users := new(GetUsersResponse)
	if bodyBytes, err := io.ReadAll(resp.Body); err != nil {
		return nil, OtherError{Msg: fmt.Sprintf("unable to read HTTP response from %v, error: %v", apiMsg, err)}
	} else if err = json.Unmarshal(bodyBytes, users); err != nil {
		return nil, OtherError{Msg: fmt.Sprintf("failed to unmarshal HTTP response from %s, error: %v", apiMsg, err)}
	}

	return users, nil
}
