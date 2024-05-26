package keeper

import (
	"sort"

	"github.com/cosmos/cosmos-sdk/codec"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	types "pkg.akt.dev/go/node/audit/v1"
	attrv1 "pkg.akt.dev/go/node/types/attributes/v1"
)

// TODO: use interfaces for keepers, queriers
type IKeeper interface {
	GetProviderByAuditor(ctx sdk.Context, id types.ProviderID) (types.Provider, bool)
	GetProviderAttributes(ctx sdk.Context, id sdk.Address) (types.Providers, bool)
	CreateOrUpdateProviderAttributes(ctx sdk.Context, id types.ProviderID, attr attrv1.Attributes) error
	DeleteProviderAttributes(ctx sdk.Context, id types.ProviderID, keys []string) error
	WithProviders(ctx sdk.Context, fn func(types.Provider) bool)
	WithProvider(ctx sdk.Context, id sdk.Address, fn func(types.Provider) bool)
}

// Keeper of the provider store
type Keeper struct {
	skey storetypes.StoreKey
	cdc  codec.BinaryCodec
}

// NewKeeper creates and returns an instance for Market keeper
func NewKeeper(cdc codec.BinaryCodec, skey storetypes.StoreKey) Keeper {
	return Keeper{cdc: cdc, skey: skey}
}

// Codec returns keeper codec
func (k Keeper) Codec() codec.BinaryCodec {
	return k.cdc
}

// StoreKey returns store key
func (k Keeper) StoreKey() storetypes.StoreKey {
	return k.skey
}

// GetProviderByAuditor returns a provider with given auditor and owner id
func (k Keeper) GetProviderByAuditor(ctx sdk.Context, id types.ProviderID) (types.Provider, bool) {
	store := ctx.KVStore(k.skey)

	buf := store.Get(providerKey(id))
	if buf == nil {
		return types.Provider{}, false
	}

	var val types.Provider
	k.cdc.MustUnmarshal(buf, &val)

	return val, true
}

// GetProviderAttributes returns a provider with given auditor and owner id's
func (k Keeper) GetProviderAttributes(ctx sdk.Context, id sdk.Address) (types.Providers, bool) {
	store := ctx.KVStore(k.skey)

	var attr types.Providers

	iter := sdk.KVStorePrefixIterator(store, providerPrefix(id))
	defer func() {
		_ = iter.Close()
	}()

	for ; iter.Valid(); iter.Next() {
		var val types.Provider
		k.cdc.MustUnmarshal(iter.Value(), &val)
		attr = append(attr, val)
	}

	if len(attr) == 0 {
		return nil, false
	}

	return attr, true
}

// CreateOrUpdateProviderAttributes update signed provider attributes.
// creates new if key does not exist
// if key exists, existing values for matching pairs will be replaced
func (k Keeper) CreateOrUpdateProviderAttributes(ctx sdk.Context, id types.ProviderID, attr attrv1.Attributes) error {
	store := ctx.KVStore(k.skey)
	key := providerKey(id)

	prov := types.Provider{
		Owner:      id.Owner.String(),
		Auditor:    id.Auditor.String(),
		Attributes: attr,
	}

	buf := store.Get(key)
	if buf != nil {
		tmp := types.Provider{}
		k.cdc.MustUnmarshal(buf, &tmp)

		kv := make(map[string]string)

		for _, entry := range tmp.Attributes {
			kv[entry.Key] = entry.Value
		}

		for _, entry := range prov.Attributes {
			kv[entry.Key] = entry.Value
		}

		attr = attrv1.Attributes{}

		for ky, val := range kv {
			attr = append(attr, attrv1.Attribute{
				Key:   ky,
				Value: val,
			})
		}

		prov.Attributes = attr
	}

	sort.SliceStable(prov.Attributes, func(i, j int) bool {
		return prov.Attributes[i].Key < prov.Attributes[j].Key
	})

	store.Set(key, k.cdc.MustMarshal(&prov))

	err := ctx.EventManager().EmitTypedEvent(
		&types.EventTrustedAuditorCreated{
			Owner:   id.Owner.String(),
			Auditor: id.Auditor.String(),
		},
	)
	if err != nil {
		return err
	}

	return nil
}

func (k Keeper) DeleteProviderAttributes(ctx sdk.Context, id types.ProviderID, keys []string) error {
	store := ctx.KVStore(k.skey)
	key := providerKey(id)

	buf := store.Get(key)
	if buf == nil {
		return types.ErrProviderNotFound
	}

	if keys == nil {
		store.Delete(key)
	} else {
		prov := types.Provider{
			Owner:   id.Owner.String(),
			Auditor: id.Auditor.String(),
		}

		tmp := types.Provider{}
		k.cdc.MustUnmarshal(buf, &tmp)

		kv := make(map[string]string)

		for _, entry := range tmp.Attributes {
			kv[entry.Key] = entry.Value
		}

		for _, entry := range keys {
			if _, exists := kv[entry]; !exists {
				return types.ErrAttributeNotFound.Wrapf("trying to delete non-existing attribute \"%s\" for auditor/provider \"%s/%s\"",
					entry,
					prov.Auditor,
					prov.Owner)
			}

			delete(kv, entry)
		}

		var attr attrv1.Attributes

		for ky, val := range kv {
			attr = append(attr, attrv1.Attribute{
				Key:   ky,
				Value: val,
			})
		}

		if len(attr) == 0 {
			store.Delete(key)
		} else {
			sort.SliceStable(attr, func(i, j int) bool {
				return attr[i].Key < attr[j].Key
			})

			prov.Attributes = attr

			store.Set(key, k.cdc.MustMarshal(&prov))
		}
	}

	err := ctx.EventManager().EmitTypedEvent(
		&types.EventTrustedAuditorDeleted{
			Owner:   id.Owner.String(),
			Auditor: id.Auditor.String(),
		},
	)
	if err != nil {
		return err
	}

	return nil
}

// WithProviders iterates all signed provider's attributes
func (k Keeper) WithProviders(ctx sdk.Context, fn func(types.Provider) bool) {
	store := ctx.KVStore(k.skey)
	iter := store.Iterator(nil, nil)

	defer func() {
		_ = iter.Close()
	}()

	for ; iter.Valid(); iter.Next() {
		var val types.Provider
		k.cdc.MustUnmarshal(iter.Value(), &val)
		if stop := fn(val); stop {
			break
		}
	}
}

// WithProviders iterates all signed provider's attributes
func (k Keeper) WithProvider(ctx sdk.Context, id sdk.Address, fn func(types.Provider) bool) {
	store := ctx.KVStore(k.skey)
	iter := sdk.KVStorePrefixIterator(store, providerPrefix(id))
	defer func() {
		_ = iter.Close()
	}()

	for ; iter.Valid(); iter.Next() {
		var val types.Provider
		k.cdc.MustUnmarshal(iter.Value(), &val)
		if stop := fn(val); stop {
			break
		}
	}
}
