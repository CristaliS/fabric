/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endorser

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/op/go-logging"
	"golang.org/x/net/context"

	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/chaincode"
	"github.com/hyperledger/fabric/core/chaincode/shim"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/core/common/validation"
	"github.com/hyperledger/fabric/core/ledger"
	"github.com/hyperledger/fabric/core/peer"
	syscc "github.com/hyperledger/fabric/core/scc"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	putils "github.com/hyperledger/fabric/protos/utils"
)

var endorserLogger = logging.MustGetLogger("endorser")

// The Jira issue that documents Endorser flow along with its relationship to
// the lifecycle chaincode - https://jira.hyperledger.org/browse/FAB-181

// Endorser provides the Endorser service ProcessProposal
type Endorser struct {
}

// NewEndorserServer creates and returns a new Endorser server instance.
func NewEndorserServer() pb.EndorserServer {
	e := new(Endorser)
	return e
}

//TODO - what would Endorser's ACL be ?
func (*Endorser) checkACL(prop *pb.Proposal) error {
	return nil
}

//TODO - check for escc and vscc
func (*Endorser) checkEsccAndVscc(prop *pb.Proposal) error {
	return nil
}

func (*Endorser) getTxSimulator(ledgername string) (ledger.TxSimulator, error) {
	lgr := peer.GetLedger(ledgername)
	if lgr == nil {
		return nil, fmt.Errorf("chain does not exist(%s)", ledgername)
	}
	return lgr.NewTxSimulator()
}

func (*Endorser) getHistoryQueryExecutor(ledgername string) (ledger.HistoryQueryExecutor, error) {
	lgr := peer.GetLedger(ledgername)
	if lgr == nil {
		return nil, fmt.Errorf("chain does not exist(%s)", ledgername)
	}
	return lgr.NewHistoryQueryExecutor()
}

//call specified chaincode (system or user)
func (e *Endorser) callChaincode(ctxt context.Context, chainID string, version string, txid string, prop *pb.Proposal, cis *pb.ChaincodeInvocationSpec, cid *pb.ChaincodeID, txsim ledger.TxSimulator) (*pb.Response, *pb.ChaincodeEvent, error) {
	var err error
	var res *pb.Response
	var ccevent *pb.ChaincodeEvent

	if txsim != nil {
		ctxt = context.WithValue(ctxt, chaincode.TXSimulatorKey, txsim)
	}

	//is this a system chaincode
	scc := syscc.IsSysCC(cid.Name)

	cccid := ccprovider.NewCCContext(chainID, cid.Name, version, txid, scc, prop)

	res, ccevent, err = chaincode.ExecuteChaincode(ctxt, cccid, cis.ChaincodeSpec.Input.Args)

	if err != nil {
		return nil, nil, err
	}

	if res.Status != shim.OK {
		return nil, nil, fmt.Errorf(string(res.Message))
	}

	//----- BEGIN -  SECTION THAT MAY NEED TO BE DONE IN LCCC ------
	//if this a call to deploy a chaincode, We need a mechanism
	//to pass TxSimulator into LCCC. Till that is worked out this
	//special code does the actual deploy, upgrade here so as to collect
	//all state under one TxSimulator
	//
	//NOTE that if there's an error all simulation, including the chaincode
	//table changes in lccc will be thrown away
	if cid.Name == "lccc" && len(cis.ChaincodeSpec.Input.Args) >= 3 && (string(cis.ChaincodeSpec.Input.Args[0]) == "deploy" || string(cis.ChaincodeSpec.Input.Args[0]) == "upgrade") {
		var cds *pb.ChaincodeDeploymentSpec
		cds, err = putils.GetChaincodeDeploymentSpec(cis.ChaincodeSpec.Input.Args[2])
		if err != nil {
			return nil, nil, err
		}

		//this should not be a system chaincode
		if syscc.IsSysCC(cds.ChaincodeSpec.ChaincodeId.Name) {
			return nil, nil, fmt.Errorf("attempting to deploy a system chaincode %s/%s", cds.ChaincodeSpec.ChaincodeId.Name, chainID)
		}

		cccid = ccprovider.NewCCContext(chainID, cds.ChaincodeSpec.ChaincodeId.Name, cds.ChaincodeSpec.ChaincodeId.Version, txid, false, prop)

		_, _, err = chaincode.Execute(ctxt, cccid, cds)
		if err != nil {
			return nil, nil, fmt.Errorf("%s", err)
		}
	}
	//----- END -------

	return res, ccevent, err
}

//simulate the proposal by calling the chaincode
func (e *Endorser) simulateProposal(ctx context.Context, chainID string, txid string, prop *pb.Proposal, cid *pb.ChaincodeID, txsim ledger.TxSimulator) (*ccprovider.ChaincodeData, *pb.Response, []byte, *pb.ChaincodeEvent, error) {
	//we do expect the payload to be a ChaincodeInvocationSpec
	//if we are supporting other payloads in future, this be glaringly point
	//as something that should change
	cis, err := putils.GetChaincodeInvocationSpec(prop)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	//---1. check ACL
	if err = e.checkACL(prop); err != nil {
		return nil, nil, nil, nil, err
	}

	//---2. check ESCC and VSCC for the chaincode
	if err = e.checkEsccAndVscc(prop); err != nil {
		return nil, nil, nil, nil, err
	}

	var cd *ccprovider.ChaincodeData

	//default it to a system CC
	version := util.GetSysCCVersion()
	if !syscc.IsSysCC(cid.Name) {
		cd, err = e.getCDSFromLCCC(ctx, chainID, txid, prop, cid.Name, txsim)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("failed to obtain cds for %s - %s", cid.Name, err)
		}
		version = cd.Version
	}

	//---3. execute the proposal and get simulation results
	var simResult []byte
	var res *pb.Response
	var ccevent *pb.ChaincodeEvent
	res, ccevent, err = e.callChaincode(ctx, chainID, version, txid, prop, cis, cid, txsim)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	if txsim != nil {
		if simResult, err = txsim.GetTxSimulationResults(); err != nil {
			return nil, nil, nil, nil, err
		}
	}

	return cd, res, simResult, ccevent, nil
}

func (e *Endorser) getCDSFromLCCC(ctx context.Context, chainID string, txid string, prop *pb.Proposal, chaincodeID string, txsim ledger.TxSimulator) (*ccprovider.ChaincodeData, error) {
	ctxt := ctx
	if txsim != nil {
		ctxt = context.WithValue(ctx, chaincode.TXSimulatorKey, txsim)
	}

	return chaincode.GetChaincodeDataFromLCCC(ctxt, txid, prop, chainID, chaincodeID)
}

//endorse the proposal by calling the ESCC
func (e *Endorser) endorseProposal(ctx context.Context, chainID string, txid string, proposal *pb.Proposal, response *pb.Response, simRes []byte, event *pb.ChaincodeEvent, visibility []byte, ccid *pb.ChaincodeID, txsim ledger.TxSimulator, cd *ccprovider.ChaincodeData) (*pb.ProposalResponse, error) {
	endorserLogger.Infof("endorseProposal starts for chainID %s, ccid %s", chainID, ccid)

	// 1) extract the name of the escc that is requested to endorse this chaincode
	var escc string
	//ie, not "lccc" or system chaincodes
	if cd != nil {
		escc = cd.Escc
		if escc == "" { // this should never happen, LCCC always fills this field
			panic("No ESCC specified in ChaincodeData")
		}
	} else {
		// FIXME: getCDSFromLCCC seems to fail for lccc - not sure this is expected?
		// TODO: who should endorse a call to LCCC?
		escc = "escc"
	}

	endorserLogger.Infof("endorseProposal info: escc for cid %s is %s", ccid, escc)

	// marshalling event bytes
	var err error
	var eventBytes []byte
	if event != nil {
		eventBytes, err = putils.GetBytesChaincodeEvent(event)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal event bytes - %s", err)
		}
	}

	resBytes, err := putils.GetBytesResponse(response)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response bytes - %s", err)
	}

	// 3) call the ESCC we've identified
	// arguments:
	// args[0] - function name (not used now)
	// args[1] - serialized Header object
	// args[2] - serialized ChaincodeProposalPayload object
	// args[3] - result of executing chaincode
	// args[4] - binary blob of simulation results
	// args[5] - serialized events
	// args[6] - payloadVisibility
	args := [][]byte{[]byte(""), proposal.Header, proposal.Payload, resBytes, simRes, eventBytes, visibility}
	version := util.GetSysCCVersion()
	ecccis := &pb.ChaincodeInvocationSpec{ChaincodeSpec: &pb.ChaincodeSpec{Type: pb.ChaincodeSpec_GOLANG, ChaincodeId: &pb.ChaincodeID{Name: escc}, Input: &pb.ChaincodeInput{Args: args}}}
	res, _, err := e.callChaincode(ctx, chainID, version, txid, proposal, ecccis, &pb.ChaincodeID{Name: escc}, txsim)
	if err != nil {
		return nil, err
	}

	if res.Status >= shim.ERROR {
		return nil, fmt.Errorf(string(res.Message))
	}

	prBytes := res.Payload
	// Note that we do not extract any simulation results from
	// the call to ESCC. This is intentional becuse ESCC is meant
	// to endorse (i.e. sign) the simulation results of a chaincode,
	// but it can't obviously sign its own. Furthermore, ESCC runs
	// on private input (its own signing key) and so if it were to
	// produce simulationr results, they are likely to be different
	// from other ESCCs, which would stand in the way of the
	// endorsement process.

	//3 -- respond
	pResp, err := putils.GetProposalResponse(prBytes)
	if err != nil {
		return nil, err
	}

	return pResp, nil
}

// ProcessProposal process the Proposal
func (e *Endorser) ProcessProposal(ctx context.Context, signedProp *pb.SignedProposal) (*pb.ProposalResponse, error) {
	// at first, we check whether the message is valid
	prop, hdr, hdrExt, err := validation.ValidateProposalMessage(signedProp)
	if err != nil {
		return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
	}

	chainID := hdr.ChannelHeader.ChannelId

	//chainless MSPs have "" chain name
	ischainless := false

	if chainID == "" {
		ischainless = true
	}

	//TODO check for uniqueness of prop.TxID with ledger

	txid := hdr.ChannelHeader.TxId
	if txid == "" {
		err = fmt.Errorf("Invalid txID")
		return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
	}

	// obtaining once the tx simulator for this proposal. This will be nil
	// for chainless proposals
	// Also obtain a history query executor for history queries, since tx simulator does not cover history
	var txsim ledger.TxSimulator
	var historyQueryExecutor ledger.HistoryQueryExecutor
	if chainID != "" {
		if txsim, err = e.getTxSimulator(chainID); err != nil {
			return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
		}
		if historyQueryExecutor, err = e.getHistoryQueryExecutor(chainID); err != nil {
			return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
		}
		// Add the historyQueryExecutor to context
		// TODO shouldn't we also add txsim to context here as well? Rather than passing txsim parameter
		// around separately, since eventually it gets added to context anyways
		ctx = context.WithValue(ctx, chaincode.HistoryQueryExecutorKey, historyQueryExecutor)

		defer txsim.Done()
	}
	//this could be a request to a chainless SysCC

	// TODO: if the proposal has an extension, it will be of type ChaincodeAction;
	//       if it's present it means that no simulation is to be performed because
	//       we're trying to emulate a submitting peer. On the other hand, we need
	//       to validate the supplied action before endorsing it

	//1 -- simulate
	cd, res, simulationResult, ccevent, err := e.simulateProposal(ctx, chainID, txid, prop, hdrExt.ChaincodeId, txsim)
	if err != nil {
		return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
	}

	//2 -- endorse and get a marshalled ProposalResponse message
	var pResp *pb.ProposalResponse

	//TODO till we implement global ESCC, CSCC for system chaincodes
	//chainless proposals (such as CSCC) don't have to be endorsed
	if ischainless {
		pResp = &pb.ProposalResponse{Response: res}
	} else {
		pResp, err = e.endorseProposal(ctx, chainID, txid, prop, res, simulationResult, ccevent, hdrExt.PayloadVisibility, hdrExt.ChaincodeId, txsim, cd)
		if err != nil {
			return &pb.ProposalResponse{Response: &pb.Response{Status: 500, Message: err.Error()}}, err
		}
	}

	// Set the proposal response payload - it
	// contains the "return value" from the
	// chaincode invocation
	pResp.Response.Payload = res.Payload

	return pResp, nil
}

// Only exposed for testing purposes - commit the tx simulation so that
// a deploy transaction is persisted and that chaincode can be invoked.
// This makes the endorser test self-sufficient
func (e *Endorser) commitTxSimulation(proposal *pb.Proposal, chainID string, signer msp.SigningIdentity, pResp *pb.ProposalResponse) error {
	tx, err := putils.CreateSignedTx(proposal, signer, pResp)
	if err != nil {
		return err
	}

	lgr := peer.GetLedger(chainID)
	if lgr == nil {
		return fmt.Errorf("failure while looking up the ledger")
	}

	txBytes, err := proto.Marshal(tx)
	if err != nil {
		return err
	}
	block := common.NewBlock(1, []byte{})
	block.Data.Data = [][]byte{txBytes}
	block.Header.DataHash = block.Data.Hash()
	if err = lgr.Commit(block); err != nil {
		return err
	}

	return nil
}
