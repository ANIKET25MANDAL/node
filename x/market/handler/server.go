package handler

import (
	"context"
	"fmt"

	"github.com/cosmos/cosmos-sdk/telemetry"
	sdk "github.com/cosmos/cosmos-sdk/types"
	v1 "pkg.akt.dev/go/node/market/v1"

	atypes "pkg.akt.dev/go/node/audit/v1"
	dbeta "pkg.akt.dev/go/node/deployment/v1beta4"
	types "pkg.akt.dev/go/node/market/v1beta5"
	ptypes "pkg.akt.dev/go/node/provider/v1beta4"
)

type msgServer struct {
	keepers Keepers
}

// NewServer returns an implementation of the market MsgServer interface
// for the provided Keeper.
func NewServer(k Keepers) types.MsgServer {
	return &msgServer{keepers: k}
}

var _ types.MsgServer = msgServer{}

func (ms msgServer) CreateBid(goCtx context.Context, msg *types.MsgCreateBid) (*types.MsgCreateBidResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	params := ms.keepers.Market.GetParams(ctx)

	minDeposit := params.BidMinDeposit
	if msg.Deposit.Denom != minDeposit.Denom {
		return nil, fmt.Errorf("%w: mininum:%v received:%v", types.ErrInvalidDeposit, minDeposit, msg.Deposit)
	}
	if minDeposit.Amount.GT(msg.Deposit.Amount) {
		return nil, fmt.Errorf("%w: mininum:%v received:%v", types.ErrInvalidDeposit, minDeposit, msg.Deposit)
	}

	if ms.keepers.Market.BidCountForOrder(ctx, msg.OrderID) > params.OrderMaxBids {
		return nil, fmt.Errorf("%w: too many existing bids (%v)", types.ErrInvalidBid, params.OrderMaxBids)
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.OrderID)
	if !found {
		return nil, types.ErrOrderNotFound
	}

	if err := order.ValidateCanBid(); err != nil {
		return nil, err
	}

	if !msg.Price.IsValid() {
		return nil, types.ErrBidInvalidPrice
	}

	if order.Price().IsLT(msg.Price) {
		return nil, types.ErrBidOverOrder
	}

	if !msg.ResourcesOffer.MatchGSpec(order.Spec) {
		return nil, types.ErrCapabilitiesMismatch
	}

	provider, err := sdk.AccAddressFromBech32(msg.Provider)
	if err != nil {
		return nil, types.ErrEmptyProvider
	}

	var prov ptypes.Provider
	if prov, found = ms.keepers.Provider.Get(ctx, provider); !found {
		return nil, types.ErrUnknownProvider
	}

	provAttr, _ := ms.keepers.Audit.GetProviderAttributes(ctx, provider)

	provAttr = append([]atypes.Provider{{
		Owner:      msg.Provider,
		Attributes: prov.Attributes,
	}}, provAttr...)

	if !order.MatchRequirements(provAttr) {
		return nil, types.ErrAttributeMismatch
	}

	if !order.MatchResourcesRequirements(prov.Attributes) {
		return nil, types.ErrCapabilitiesMismatch
	}

	bid, err := ms.keepers.Market.CreateBid(ctx, msg.OrderID, provider, msg.Price, msg.ResourcesOffer)
	if err != nil {
		return nil, err
	}

	// create escrow account for this bid
	if err := ms.keepers.Escrow.AccountCreate(ctx,
		types.EscrowAccountForBid(bid.ID),
		provider,
		provider, // bids currently don't support deposits by non-owners
		msg.Deposit); err != nil {
		return &types.MsgCreateBidResponse{}, err
	}

	telemetry.IncrCounter(1.0, "akash.bids")
	return &types.MsgCreateBidResponse{}, nil
}

func (ms msgServer) CloseBid(goCtx context.Context, msg *types.MsgCloseBid) (*types.MsgCloseBidResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	bid, found := ms.keepers.Market.GetBid(ctx, msg.ID)
	if !found {
		return nil, types.ErrUnknownBid
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, types.ErrUnknownOrderForBid
	}

	if bid.State == v1.BidOpen {
		_ = ms.keepers.Market.OnBidClosed(ctx, bid)
		return &types.MsgCloseBidResponse{}, nil
	}

	lease, found := ms.keepers.Market.GetLease(ctx, v1.LeaseID(msg.ID))
	if !found {
		return nil, types.ErrUnknownLeaseForBid
	}

	if lease.State != v1.LeaseActive {
		return nil, types.ErrLeaseNotActive
	}

	if bid.State != v1.BidActive {
		return nil, types.ErrBidNotActive
	}

	if err := ms.keepers.Deployment.OnBidClosed(ctx, order.ID.GroupID()); err != nil {
		return nil, err
	}

	_ = ms.keepers.Market.OnLeaseClosed(ctx, lease, v1.LeaseClosed)
	_ = ms.keepers.Market.OnBidClosed(ctx, bid)
	_ = ms.keepers.Market.OnOrderClosed(ctx, order)

	_ = ms.keepers.Escrow.PaymentClose(ctx,
		dbeta.EscrowAccountForDeployment(lease.ID.DeploymentID()),
		types.EscrowPaymentForLease(lease.ID))

	telemetry.IncrCounter(1.0, "akash.order_closed")

	return &types.MsgCloseBidResponse{}, nil
}

func (ms msgServer) WithdrawLease(goCtx context.Context, msg *types.MsgWithdrawLease) (*types.MsgWithdrawLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	_, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return nil, types.ErrUnknownLease
	}

	if err := ms.keepers.Escrow.PaymentWithdraw(ctx,
		dbeta.EscrowAccountForDeployment(msg.ID.DeploymentID()),
		types.EscrowPaymentForLease(msg.ID),
	); err != nil {
		return &types.MsgWithdrawLeaseResponse{}, err
	}

	return &types.MsgWithdrawLeaseResponse{}, nil
}

func (ms msgServer) CreateLease(goCtx context.Context, msg *types.MsgCreateLease) (*types.MsgCreateLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	bid, found := ms.keepers.Market.GetBid(ctx, msg.BidID)
	if !found {
		return &types.MsgCreateLeaseResponse{}, types.ErrBidNotFound
	}

	if bid.State != v1.BidOpen {
		return &types.MsgCreateLeaseResponse{}, types.ErrBidNotOpen
	}

	order, found := ms.keepers.Market.GetOrder(ctx, msg.BidID.OrderID())
	if !found {
		return &types.MsgCreateLeaseResponse{}, types.ErrOrderNotFound
	}

	if order.State != v1.OrderOpen {
		return &types.MsgCreateLeaseResponse{}, types.ErrOrderNotOpen
	}

	group, found := ms.keepers.Deployment.GetGroup(ctx, order.ID.GroupID())
	if !found {
		return &types.MsgCreateLeaseResponse{}, types.ErrGroupNotFound
	}

	if group.State != dbeta.GroupOpen {
		return &types.MsgCreateLeaseResponse{}, types.ErrGroupNotOpen
	}

	owner, err := sdk.AccAddressFromBech32(msg.BidID.Provider)
	if err != nil {
		return &types.MsgCreateLeaseResponse{}, err
	}

	if err := ms.keepers.Escrow.PaymentCreate(ctx,
		dbeta.EscrowAccountForDeployment(msg.BidID.DeploymentID()),
		types.EscrowPaymentForLease(msg.BidID.LeaseID()),
		owner,
		bid.Price); err != nil {
		return &types.MsgCreateLeaseResponse{}, err
	}

	_ = ms.keepers.Market.CreateLease(ctx, bid)
	ms.keepers.Market.OnOrderMatched(ctx, order)
	ms.keepers.Market.OnBidMatched(ctx, bid)

	// close losing bids
	var lostbids []types.Bid
	ms.keepers.Market.WithBidsForOrder(ctx, msg.BidID.OrderID(), func(bid types.Bid) bool {
		if bid.ID.Equals(msg.BidID) {
			return false
		}
		if bid.State != v1.BidOpen {
			return false
		}

		lostbids = append(lostbids, bid)
		return false
	})

	for _, bid := range lostbids {
		ms.keepers.Market.OnBidLost(ctx, bid)
		if err := ms.keepers.Escrow.AccountClose(ctx,
			types.EscrowAccountForBid(bid.ID)); err != nil {
			return &types.MsgCreateLeaseResponse{}, err
		}
	}

	return &types.MsgCreateLeaseResponse{}, nil
}

func (ms msgServer) CloseLease(goCtx context.Context, msg *types.MsgCloseLease) (*types.MsgCloseLeaseResponse, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	order, found := ms.keepers.Market.GetOrder(ctx, msg.ID.OrderID())
	if !found {
		return nil, types.ErrOrderNotFound
	}

	if order.State != v1.OrderActive {
		return &types.MsgCloseLeaseResponse{}, types.ErrOrderClosed
	}

	bid, found := ms.keepers.Market.GetBid(ctx, msg.ID.BidID())
	if !found {
		return &types.MsgCloseLeaseResponse{}, types.ErrBidNotFound
	}
	if bid.State != v1.BidActive {
		return &types.MsgCloseLeaseResponse{}, types.ErrBidNotActive
	}

	lease, found := ms.keepers.Market.GetLease(ctx, msg.ID)
	if !found {
		return &types.MsgCloseLeaseResponse{}, types.ErrLeaseNotFound
	}
	if lease.State != v1.LeaseActive {
		return &types.MsgCloseLeaseResponse{}, types.ErrOrderClosed
	}

	_ = ms.keepers.Market.OnLeaseClosed(ctx, lease, v1.LeaseClosed)
	_ = ms.keepers.Market.OnBidClosed(ctx, bid)
	_ = ms.keepers.Market.OnOrderClosed(ctx, order)

	if err := ms.keepers.Escrow.PaymentClose(ctx,
		dbeta.EscrowAccountForDeployment(lease.ID.DeploymentID()),
		types.EscrowPaymentForLease(lease.ID),
	); err != nil {
		return &types.MsgCloseLeaseResponse{}, err
	}

	group, err := ms.keepers.Deployment.OnLeaseClosed(ctx, msg.ID.GroupID())
	if err != nil {
		return &types.MsgCloseLeaseResponse{}, err
	}

	if group.State != dbeta.GroupOpen {
		return &types.MsgCloseLeaseResponse{}, nil
	}
	if _, err := ms.keepers.Market.CreateOrder(ctx, group.ID, group.GroupSpec); err != nil {
		return &types.MsgCloseLeaseResponse{}, err
	}
	return &types.MsgCloseLeaseResponse{}, nil

}
