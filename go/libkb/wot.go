package libkb

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/keybase/client/go/protocol/keybase1"
)

func getWotVouchChainLink(mctx MetaContext, uid keybase1.UID, sigID keybase1.SigID) (cl *WotVouchChainLink, voucher *User, err error) {
	user, err := LoadUser(NewLoadUserArgWithMetaContext(mctx).WithUID(uid))
	if err != nil {
		return nil, nil, fmt.Errorf("Error loading user: %v", err)
	}
	link := user.LinkFromSigID(sigID)
	if link == nil {
		return nil, nil, fmt.Errorf("Could not find link from sigID")
	}
	if link.revoked {
		return nil, nil, fmt.Errorf("Link is revoked")
	}

	tlink, w := NewTypedChainLink(link)
	if w != nil {
		return nil, nil, fmt.Errorf("Could not get typed chain link: %v", w.Warning())
	}

	vlink, ok := tlink.(*WotVouchChainLink)
	if !ok {
		return nil, nil, fmt.Errorf("Link is not a WotVouchChainLink: %v", tlink)
	}
	return vlink, user, nil
}

func assertVouchIsForMe(mctx MetaContext, vouchedUser wotExpansionUser) (err error) {
	me, err := LoadMe(NewLoadUserArgWithMetaContext(mctx))
	if err != nil {
		return fmt.Errorf("error loading myself: %s", err.Error())
	}
	if me.GetName() != vouchedUser.Username {
		return fmt.Errorf("wot username isn't me %s != %s", me.GetName(), vouchedUser.Username)
	}
	if me.GetUID() != vouchedUser.UID {
		return fmt.Errorf("wot uid isn't me %s != %s", me.GetUID(), vouchedUser.UID)
	}
	if me.GetEldestKID() != vouchedUser.Eldest.KID {
		return fmt.Errorf("wot eldest kid isn't me %s != %s", me.GetEldestKID(), vouchedUser.Eldest.KID)
	}
	return nil
}

type wotExpansionUser struct {
	Eldest struct {
		KID   keybase1.KID
		Seqno keybase1.Seqno
	}
	SeqTail struct {
		PayloadHash string
		Seqno       keybase1.Seqno
		SigID       string
	}
	UID      keybase1.UID
	Username string
}

// standardizeConfidence converts the confidence blob that's in the sig expansion into
// the keybase1.Confidence type
func standardizeConfidence(mctx MetaContext, expansionConfidence map[string]interface{}) (*keybase1.Confidence, error) {
	var confidence keybase1.Confidence
	// reach into expansionConfidence and change username_verified_via if present
	if verificationRaw, ok := expansionConfidence["username_verified_via"]; ok {
		// replace "video" -> "VIDEO" -> 2
		verification, ok := verificationRaw.(string)
		if !ok {
			return nil, fmt.Errorf("cannot convert %v into UsernameVerifiedVia", verificationRaw)
		}
		expansionConfidence["username_verified_via"] = keybase1.UsernameVerificationTypeMap[strings.ToUpper(verification)]
	}
	// reach into expansionConfidence and change vouched_by if present
	if vouchedByRaw, ok := expansionConfidence["vouched_by"]; ok {
		// replace user []uids -> []username
		vouchedByUIDs, ok := vouchedByRaw.([]interface{})
		if !ok {
			return nil, fmt.Errorf("cannot convert %v into list of uids", vouchedByUIDs)
		}
		var vouchedByUsernames []string
		for _, vuid := range vouchedByUIDs {
			uid, ok := vuid.(string)
			if !ok {
				return nil, fmt.Errorf("cannot convert %v into uid", vuid)
			}
			username, err := mctx.G().GetUPAKLoader().LookupUsername(mctx.Ctx(), keybase1.UID(uid))
			if err != nil {
				return nil, err
			}
			vouchedByUsernames = append(vouchedByUsernames, username.String())
		}
		expansionConfidence["vouched_by"] = vouchedByUsernames
	}
	// now expansionConfidence should match keybase1.Confidence, so serialize and deserialize
	// to do the recursive type conversion
	asJsonBytes, err := json.Marshal(expansionConfidence)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(asJsonBytes, &confidence)
	if err != nil {
		return nil, err
	}
	return &confidence, nil
}

type wotExpansionDetails struct {
	User                wotExpansionUser       `json:"user"`
	ExpansionConfidence map[string]interface{} `json:"confidence"`
	VouchTexts          []string               `json:"vouch_text"`
}

func transformPending(mctx MetaContext, serverPending apiPendingWot) (res keybase1.PendingVouch, err error) {
	// load the voucher and fetch the relevant chain link
	wotVouchLink, voucher, err := getWotVouchChainLink(mctx, serverPending.UID, serverPending.SigID)
	if err != nil {
		return res, fmt.Errorf("error finding the pending vouch in the voucher's sigchain: %s", err.Error())
	}
	// extract the sig expansion
	expansionObject, err := ExtractExpansionObj(wotVouchLink.ExpansionID, serverPending.ExpansionJSON)
	if err != nil {
		return res, fmt.Errorf("error extracting and validating the expansion: %s", err.Error())
	}
	// load it into the right type for web-of-trust vouching
	var wotObj wotExpansionDetails
	err = json.Unmarshal(expansionObject, &wotObj)
	if err != nil {
		return res, fmt.Errorf("error casting expansion object to expected web-of-trust schema: %s", err.Error())
	}
	err = assertVouchIsForMe(mctx, wotObj.User)
	if err != nil {
		mctx.Debug("web-of-trust pending vouch user-section doesn't look right: %+v", wotObj.User)
		return res, fmt.Errorf("error verifying user section of web-of-trust expansion: %s", err.Error())
	}
	// convert the confidence object that's in the expansion to the standard type in keybase1
	confidence, err := standardizeConfidence(mctx, wotObj.ExpansionConfidence)
	if err != nil {
		return res, fmt.Errorf("error standardizing confidence: %s", err.Error())
	}
	// build a PendingVouch
	vouch := keybase1.PendingVouch{
		Voucher:    voucher.ToUserVersion(),
		Proof:      serverPending.SigID,
		VouchTexts: wotObj.VouchTexts,
		Confidence: *confidence,
	}
	return vouch, nil
}

type apiPendingWot struct {
	UID           keybase1.UID   `json:"voucher"`
	EldestSeqno   keybase1.Seqno `json:"voucher_eldest_seqno"`
	SigID         keybase1.SigID `json:"sig_id"`
	ExpansionJSON string         `json:"expansion_json"`
}

type GetPendingWotVouches struct {
	AppStatusEmbed
	Pending []apiPendingWot `json:"pending"`
}

func FetchPendingWotVouches(mctx MetaContext) (res []keybase1.PendingVouch, err error) {
	defer mctx.Trace("FetchPendingWotVouches", func() error { return err })()
	apiArg := APIArg{
		Endpoint:    "wot/pending",
		SessionType: APISessionTypeREQUIRED,
	}
	var response GetPendingWotVouches
	err = mctx.G().API.GetDecode(mctx, apiArg, &response)
	if err != nil {
		mctx.Debug("error fetching pending web-of-trust vouches: %s", err.Error())
		return nil, err
	}
	for _, apiPending := range response.Pending {
		newPending, err := transformPending(mctx, apiPending)
		if err != nil {
			mctx.Debug("error validating server-reported pending web-of-trust vouches: %s", err.Error())
			return nil, err
		}
		res = append(res, newPending)
	}
	mctx.Debug("found %d pending web-of-trust vouches", len(res))
	return res, nil
}
