package admin

import (
	"encoding/binary"
	"errors"
	"strings"

	"go.dedis.ch/cothority/v3/byzcoin"
	"go.dedis.ch/cothority/v3/byzcoin/bcadmin/lib"
	"go.dedis.ch/cothority/v3/darc"
	"go.dedis.ch/cothority/v3/darc/expression"
	"go.dedis.ch/protobuf"
	"golang.org/x/xerrors"
)

type Client struct {
	bcl           *byzcoin.Client
	adminkeys     darc.Signer
	genDarc       darc.Darc
	signerCounter uint64
}

var adminActions = map[darc.Action]uint{
	"invoke:darc.evolve":             0,
	"spawn:deferred":                 1,
	"invoke:deferred.addProof":       1,
	"invoke:deferred.execProposedTx": 1,
	"spawn:darc":                     0,
}

func NewClient(bcl *byzcoin.Client) (*Client, error) {
	if bcl == nil {
		return nil, errors.New("A Byzcoin Client is required")
	}
	cl := &Client{
		bcl:           bcl,
		adminkeys:     darc.NewSignerEd25519(nil, nil), // TODO add as optional arguments
		signerCounter: 1,
	}
	if genDarc, err := bcl.GetGenDarc(); err == nil {
		cl.genDarc = *genDarc
		return cl, nil
	} else {
		return nil, xerrors.Errorf("getting genesis darc from chain: %w", err)
	}
}

func NewClientWithAuth(bcl *byzcoin.Client, keys *darc.Signer) (*Client, error) {
	if keys == nil {
		return nil, errors.New("Keys are required")
	}
	cl, err := NewClient(bcl)
	cl.adminkeys = *keys
	return cl, err
}

func (cl *Client) AuthKey() darc.Signer {
	return cl.adminkeys
}

func (cl *Client) SpawnNewAdminDarc() (*darc.Darc, error) {
	adminDarc, err := cl.createAdminDarc()
	if err != nil {
		return nil, xerrors.Errorf("Creating the admin darc: %w", err)
	}
	buf, err := adminDarc.ToProto()
	if err != nil {
		return nil, xerrors.Errorf("Marshalling: %w", err)
	}
	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NewInstanceID(cl.genDarc.GetBaseID()),
		Spawn: &byzcoin.Spawn{
			ContractID: byzcoin.ContractDarcID,
			Args: byzcoin.Arguments{
				{
					Name:  "darc",
					Value: buf,
				},
			},
		},
		// SignerIdentities: []darc.Identity{superAdmin.Identity()},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return nil, xerrors.Errorf("Creating the deffered transaction: %w", err)
	}
	err = cl.spawnTransaction(ctx)
	if err != nil {
		return nil, xerrors.Errorf("Adding transaction to the ledger: %w", err)
	}
	return adminDarc, err

}

// TODO will need to use the method to create threshold multisig rules when implemented
func createMultisigRuleExpression(al []string) expression.Expr {
	return expression.InitAndExpr(al...) // For now everyone needs to sign
}

func (cl *Client) createAdminDarc() (*darc.Darc, error) {
	description := "Admin darc guards medchain project darcs"
	rules := darc.InitRules([]darc.Identity{cl.adminkeys.Identity()}, []darc.Identity{cl.adminkeys.Identity()})
	adminDarc := darc.NewDarc(rules, []byte(description))
	adminDarcActions := "invoke:darc.evolve,spawn:deferred,invoke:deferred.addProof,invoke:deferred.execProposedTx,spawn:darc"
	adminDarcExpr := createMultisigRuleExpression([]string{cl.adminkeys.Identity().String()})
	err := AddRuleToDarc(adminDarc, adminDarcActions, adminDarcExpr)
	return adminDarc, err
}

func (cl *Client) addDeferredTransaction(tx byzcoin.ClientTransaction, adid darc.ID) (byzcoin.InstanceID, error) {
	txBuf, err := protobuf.Encode(&tx)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Marshalling the transaction: %w", err)
	}
	ctxID, err := cl.spawnDeferredInstance(txBuf, adid)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Creating the deffered transaction: %w", err)
	}
	return ctxID, nil
}

func (cl *Client) AddAdminToAdminDarc(adid darc.ID, newAdmin darc.Identity) (byzcoin.InstanceID, error) {
	adminDarc, err := lib.GetDarcByID(cl.bcl, adid)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Getting the admin darc from chain: %w", err)
	}
	exp := adminDarc.Rules.GetEvolutionExpr()
	slc := strings.Split(string(exp), "&")
	for i := range slc {
		slc[i] = strings.TrimSpace(slc[i])
	}
	slc = append(slc, newAdmin.String())
	proposedTransaction, err := cl.evolveAdminDarc(slc, adminDarc)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Evolving the admin darc: %w", err)
	}

	return cl.addDeferredTransaction(proposedTransaction, adid)
}

func (cl *Client) RemoveAdminFromAdminDarc(adid darc.ID, adminId darc.Identity) (byzcoin.InstanceID, error) {
	adminDarc, err := lib.GetDarcByID(cl.bcl, adid)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Getting the admin darc from chain: %w", err)
	}
	exp := adminDarc.Rules.GetEvolutionExpr()
	slc := strings.Split(string(exp), "&")
	for i := range slc {
		slc[i] = strings.TrimSpace(slc[i])
	}
	idx := IndexOf(adminId.String(), slc)
	if idx == -1 {
		return *new(byzcoin.InstanceID), xerrors.Errorf("The adminID doesn't exist in the admin darc")
	}
	slc = append(slc[:idx], slc[idx+1:]...)
	proposedTransaction, err := cl.evolveAdminDarc(slc, adminDarc)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Evolving the admin darc: %w", err)
	}
	return cl.addDeferredTransaction(proposedTransaction, adid)
}

func (cl *Client) ModifyAdminKeysFromAdminDarc(adid darc.ID, oldkey, newkey darc.Identity) (byzcoin.InstanceID, error) {
	adminDarc, err := lib.GetDarcByID(cl.bcl, adid)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Getting the admin darc from chain: %w", err)
	}
	exp := adminDarc.Rules.GetEvolutionExpr()
	slc := strings.Split(string(exp), "&")
	for i := range slc {
		slc[i] = strings.TrimSpace(slc[i])
	}
	idx := IndexOf(oldkey.String(), slc)
	if idx == -1 {
		return *new(byzcoin.InstanceID), xerrors.Errorf("The adminID doesn't exist in the admin darc")
	}
	slc[idx] = newkey.String()
	proposedTransaction, err := cl.evolveAdminDarc(slc, adminDarc)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Evolving the admin darc: %w", err)
	}
	return cl.addDeferredTransaction(proposedTransaction, adid)
}

func (cl *Client) updateAdminRules(evolvedAdminDarc *darc.Darc, newAdminExpr []expression.Expr) error {
	err := evolvedAdminDarc.Rules.UpdateEvolution(newAdminExpr[0])
	if err != nil {
		return xerrors.Errorf("Updating the _evolve expression in admin darc: %w", err)
	}
	err = evolvedAdminDarc.Rules.UpdateSign(newAdminExpr[0])
	if err != nil {
		return xerrors.Errorf("Updating the _sign expression in admin darc: %w", err)
	}

	for k, v := range adminActions {
		err = evolvedAdminDarc.Rules.UpdateRule(k, newAdminExpr[v])
		if err != nil {
			return xerrors.Errorf("Updating the %s expression in admin darc: %w", k, err)
		}
	}
	return nil
}

func (cl *Client) evolveAdminDarc(slc []string, olddarc *darc.Darc) (byzcoin.ClientTransaction, error) {
	newdarc := olddarc.Copy()
	newAdminExpr := []expression.Expr{createMultisigRuleExpression(slc), expression.InitOrExpr(slc...)}
	err := cl.updateAdminRules(newdarc, newAdminExpr)
	if err != nil {
		return byzcoin.ClientTransaction{}, xerrors.Errorf("Updating admin rules: %w", err)
	}
	err = newdarc.EvolveFrom(olddarc)
	if err != nil {
		return byzcoin.ClientTransaction{}, xerrors.Errorf("Evolving the admin darc: %w", err)
	}
	_, darc2Buf, err := newdarc.MakeEvolveRequest(cl.AuthKey())
	if err != nil {
		return byzcoin.ClientTransaction{}, xerrors.Errorf("Creating the evolution request: %w", err)
	}

	proposedTransaction, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NewInstanceID(olddarc.GetBaseID()),
		Invoke: &byzcoin.Invoke{
			ContractID: byzcoin.ContractDarcID,
			Command:    "evolve",
			Args: []byzcoin.Argument{{
				Name:  "darc",
				Value: darc2Buf,
			}},
		},
	})
	if err != nil {
		return byzcoin.ClientTransaction{}, xerrors.Errorf("Creating the transaction: %w", err)
	}
	return proposedTransaction, nil
}

func (cl *Client) spawnDeferredInstance(proposedTransactionBuf []byte, adid darc.ID) (byzcoin.InstanceID, error) {
	// TODO add as arguments
	expireBlockIndexInt := uint64(6000)
	expireBlockIndexBuf := make([]byte, 8)
	binary.LittleEndian.PutUint64(expireBlockIndexBuf, expireBlockIndexInt)

	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NewInstanceID(adid),
		Spawn: &byzcoin.Spawn{
			ContractID: byzcoin.ContractDeferredID,
			Args: []byzcoin.Argument{
				{
					Name:  "proposedTransaction",
					Value: proposedTransactionBuf,
				},
				{
					Name:  "expireBlockIndex",
					Value: expireBlockIndexBuf,
				},
			},
		},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Creating the deffered transaction: %w", err)
	}
	err = cl.spawnTransaction(ctx)
	if err != nil {
		return *new(byzcoin.InstanceID), xerrors.Errorf("Adding transaction to the ledger: %w", err)
	}
	return ctx.Instructions[0].DeriveID(""), err
}

func (cl *Client) AddSignatureToDefferedTx(instID byzcoin.InstanceID) error {
	result, err := cl.bcl.GetDeferredData(instID)
	if err != nil {
		return xerrors.Errorf("Getting the deffered instance from chain: %w", err)
	}
	rootHash := result.InstructionHashes
	identity := cl.AuthKey().Identity()
	identityBuf, err := protobuf.Encode(&identity)
	if err != nil {
		return xerrors.Errorf("Encoding the identity of signer: %w", err)
	}
	signature, err := cl.AuthKey().Sign(rootHash[0]) // == index
	if err != nil {
		return xerrors.Errorf("Signing the deffered transaction: %w", err)
	}
	// TODO: Implement multi instructions transactions.
	index := uint32(0) // The index of the instruction to sign in the transaction
	indexBuf := make([]byte, 4)
	binary.LittleEndian.PutUint32(indexBuf, uint32(index))

	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: instID,
		Invoke: &byzcoin.Invoke{
			ContractID: byzcoin.ContractDeferredID,
			Command:    "addProof",
			Args: []byzcoin.Argument{
				{
					Name:  "identity",
					Value: identityBuf,
				},
				{
					Name:  "signature",
					Value: signature,
				},
				{
					Name:  "index",
					Value: indexBuf,
				},
			},
		},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return xerrors.Errorf("Creating the transaction: %w", err)
	}
	return cl.spawnTransaction(ctx)
}

func (cl *Client) ExecDefferedTx(instID byzcoin.InstanceID) error {
	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: instID,
		Invoke: &byzcoin.Invoke{
			ContractID: byzcoin.ContractDeferredID,
			Command:    "execProposedTx",
		},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return xerrors.Errorf("Creating the transaction: %w", err)
	}
	return cl.spawnTransaction(ctx)
}

func (cl *Client) CreateNewProject(adid darc.ID, pname string) (darc.ID, error) {
	pdarcID, err := cl.createProjectDarc(pname, adid)
	if err != nil {
		return nil, xerrors.Errorf("Creating the project darc: %w", err)
	}
	err = cl.createAccessRight(adid, pdarcID)
	if err != nil {
		return nil, xerrors.Errorf("Creating the access right: %w", err)
	}
	return pdarcID, err
}

func (cl *Client) createProjectDarc(pname string, adid darc.ID) (darc.ID, error) {
	pdarcDescription := pname
	rules := darc.InitRules([]darc.Identity{cl.adminkeys.Identity()}, []darc.Identity{cl.adminkeys.Identity()})
	pdarc := darc.NewDarc(rules, []byte(pdarcDescription))
	pdarcActions := "_name:value,spawn:value,invoke:value.update" //TODO arg ?
	pdarcExpr := createMultisigRuleExpression([]string{cl.adminkeys.Identity().String()})
	err := AddRuleToDarc(pdarc, pdarcActions, pdarcExpr)

	if err != nil {
		return darc.ID{}, xerrors.Errorf("Adding rules to darc: %w", err)
	}
	pdarcProto, err := pdarc.ToProto()
	if err != nil {
		return darc.ID{}, xerrors.Errorf("Marshalling: %w", err)
	}
	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NewInstanceID(adid),
		Spawn: &byzcoin.Spawn{
			ContractID: byzcoin.ContractDarcID,
			Args: byzcoin.Arguments{
				{
					Name:  "darc",
					Value: pdarcProto,
				},
			},
		},
		// SignerIdentities: []darc.Identity{superAdmin.Identity()},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return darc.ID{}, xerrors.Errorf("Creating the transaction: %w", err)
	}
	err = cl.spawnTransaction(ctx)
	if err != nil {
		return darc.ID{}, xerrors.Errorf("Adding transaction to the ledger: %w", err)
	}
	return pdarc.GetBaseID(), nil
}

func (cl *Client) createAccessRight(adid, pdarc darc.ID) error {
	emptyAccess := AccessRight{make(map[string]string)}
	emptyAccess.AccessRightsMap["init"] = "init" //TODO find another way to initialize the access right with a default value
	buf, err := protobuf.Encode(&emptyAccess)
	if err != nil {
		return xerrors.Errorf("Encoding the access right struct: %w", err)
	}
	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NewInstanceID(pdarc),
		Spawn: &byzcoin.Spawn{
			ContractID: "value",
			Args: byzcoin.Arguments{
				byzcoin.Argument{
					Name:  "value",
					Value: buf,
				},
			},
		},
		SignerCounter: []uint64{cl.signerCounter},
	})

	if err != nil {
		return xerrors.Errorf("Creating the transaction: %w", err)
	}
	err = cl.spawnTransaction(ctx)
	if err != nil {
		return xerrors.Errorf("Adding transaction to the ledger: %w", err)
	}
	instID := ctx.Instructions[0].DeriveID("")
	ctx, err = cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: byzcoin.NamingInstanceID,
		Invoke: &byzcoin.Invoke{
			ContractID: byzcoin.ContractNamingID,
			Command:    "add",
			Args: byzcoin.Arguments{
				{
					Name:  "instanceID",
					Value: instID.Slice(),
				},
				{
					Name:  "name",
					Value: []byte("AR"),
				},
			},
		},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return xerrors.Errorf("Creating the transaction: %w", err)
	}
	return cl.spawnTransaction(ctx)
}

func (cl *Client) getLastSignerCounter() uint64 {
	signerCtrs, _ := cl.bcl.GetSignerCounters(cl.AuthKey().Identity().String())
	return signerCtrs.Counters[0]
}

func (cl *Client) incrementSignerCounter() {
	cl.signerCounter++
}

func (cl *Client) updateSignerCounter(sc uint64) {
	cl.signerCounter = sc
}

func IndexOf(rule string, rules []string) int {
	for k, v := range rules {
		if rule == v {
			return k
		}
	}
	return -1
}

func (cl *Client) GetAccessRightFromProjectDarcID(pdid darc.ID) (*AccessRight, byzcoin.InstanceID, error) {
	arid, err := cl.bcl.ResolveInstanceID(pdid, "AR") // check that the access right value contract is correctly named
	if err != nil {
		return &AccessRight{}, byzcoin.InstanceID{}, xerrors.Errorf("Resolving the instance id of access right instance: %w", err)
	}
	pr, err := cl.bcl.GetProof(arid.Slice())
	if err != nil {
		return &AccessRight{}, byzcoin.InstanceID{}, xerrors.Errorf("Resolving the proof of the access right instance: %w", err)
	}
	v0, _, _, err := pr.Proof.Get(arid.Slice())
	if err != nil {
		return &AccessRight{}, byzcoin.InstanceID{}, xerrors.Errorf("Getting the proof of access right: %w", err)
	}
	ar := AccessRight{}
	err = protobuf.Decode(v0, &ar)
	if err != nil {
		return &AccessRight{}, byzcoin.InstanceID{}, xerrors.Errorf("Encoding: %w", err)
	}
	return &ar, arid, nil
}

func (cl *Client) AddQuerierToProject(pdid darc.ID, qid, access string) error {
	ar, arid, err := cl.GetAccessRightFromProjectDarcID(pdid)
	if err != nil {
		return xerrors.Errorf("Getting the access rights from project darc: %w", err)
	}
	if _, ok := ar.AccessRightsMap[qid]; ok {
		return xerrors.Errorf("The querier already exist in the access rights : %w", err)
	}
	ar.AccessRightsMap[qid] = access
	buf, err := protobuf.Encode(ar)
	if err != nil {
		return xerrors.Errorf("Encoding the access right struct: %w", err)
	}
	err = cl.updateValue(buf, arid)
	if err != nil {
		return xerrors.Errorf("Updating the access right: %w", err)
	}
	return nil
}

func (cl *Client) RemoveQuerierFromProject(pdid darc.ID, qid string) error {
	ar, arid, err := cl.GetAccessRightFromProjectDarcID(pdid)
	if err != nil {
		return xerrors.Errorf("Getting the access rights from project darc: %w", err)
	}
	delete(ar.AccessRightsMap, qid)
	buf, err := protobuf.Encode(ar)
	if err != nil {
		return xerrors.Errorf("Encoding the access right struct: %w", err)
	}
	err = cl.updateValue(buf, arid)
	if err != nil {
		return xerrors.Errorf("Updating the access right: %w", err)
	}
	return nil
}

func (cl *Client) ModifyQuerierAccessRightsForProject(pdid darc.ID, qid, access string) error {
	ar, arid, err := cl.GetAccessRightFromProjectDarcID(pdid)
	if err != nil {
		return xerrors.Errorf("Getting the access rights from project darc: %w", err)
	}
	if _, ok := ar.AccessRightsMap[qid]; !ok {
		return xerrors.Errorf("The querier doesn't exist in the access rights  : %w", err)
	}
	ar.AccessRightsMap[qid] = access
	buf, err := protobuf.Encode(&ar)
	if err != nil {
		return xerrors.Errorf("Encoding the access right struct: %w", err)
	}
	err = cl.updateValue(buf, arid)
	if err != nil {
		return xerrors.Errorf("Updating the access right: %w", err)
	}
	return nil
}

func (cl *Client) updateValue(v []byte, vcid byzcoin.InstanceID) error {
	ctx, err := cl.bcl.CreateTransaction(byzcoin.Instruction{
		InstanceID: vcid,
		Invoke: &byzcoin.Invoke{
			ContractID: ContractValueID,
			Command:    "update",
			Args: []byzcoin.Argument{{
				Name:  "value",
				Value: v,
			}},
		},
		SignerCounter: []uint64{cl.signerCounter},
	})
	if err != nil {
		return xerrors.Errorf("Creating the transaction: %w", err)
	}
	return cl.spawnTransaction(ctx)
}

func (cl *Client) spawnTransaction(ctx byzcoin.ClientTransaction) error {
	err := ctx.FillSignersAndSignWith(cl.adminkeys)
	if err != nil {
		return xerrors.Errorf("Signing: %w", err)
	}
	_, err = cl.bcl.AddTransactionAndWait(ctx, 10)
	if err != nil {
		return xerrors.Errorf("Adding transaction to the ledger: %w", err)
	}
	cl.incrementSignerCounter()
	return nil
}

// TODO Create util package to reuse methods
func AddRuleToDarc(userDarc *darc.Darc, action string, expr expression.Expr) error {
	actions := strings.Split(action, ",")

	for i := 0; i < len(actions); i++ {
		dAction := darc.Action(actions[i])
		err := userDarc.Rules.AddRule(dAction, expr)
		if err != nil {
			return err
		}
	}
	return nil
}
