// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/// @title ReservedBlockspaceRegistry
/// @notice Registry for reserved blockspace clients and their whitelisted sender addresses.
/// @dev This contract has no constructor so its deployed runtime bytecode can be embedded directly in genesis.
/// @dev TODO: this source lives in-tree only so bor can regenerate and embed the runtime bytecode. Its canonical
///      home is the Polygon genesis-contracts repo (contracts-team owned); once it moves there, bor should consume
///      the embedded bytecode from that repo rather than carrying the foundry project here.
contract ReservedBlockspaceRegistry {
    error AlreadyInitialized();
    error NotInitialized();
    error NotOwner();
    error NotClientAdmin();
    error ZeroAddress();
    error UnknownClient();
    error AddressAlreadyRegistered();
    error AddressNotRegistered();
    error InvalidQuota();
    error InvalidFeeMode();
    error MaxTotalReservedGasExceeded();
    error MaxClientReservedGasExceeded();

    struct Client {
        address admin;
        uint64 gasQuota;
        bool active;
        // feeMode: 0 = free (zero in-protocol fee), 1 = routed (fee paid but
        // credited to the producer). Routed mode is not implemented by the
        // execution client: it excludes non-free clients from the reserved
        // set, so their senders pay standard fees like normal transactions.
        uint8 feeMode;
        // effectiveFrom: block number from which this client's reserved status
        // applies. Lets governance schedule/announce a change at a future height
        // (a kill-switch via setClientActive(false) is still immediate).
        uint64 effectiveFrom;
        string metadata;
        address[] addresses;
    }

    uint8 internal constant FEE_MODE_FREE = 0;
    uint8 internal constant FEE_MODE_ROUTED = 1;

    address public owner;
    uint64 public maxTotalReservedGas;
    uint64 public maxClientReservedGas;
    uint256 public nextClientId;
    uint64 public totalReservedGas;

    // configVersion increments on every change to the reserved set or its
    // limits. Bor reads root() once per block and only rebuilds its cached
    // snapshot when the value changes, avoiding a per-transaction state read.
    uint256 public configVersion;

    mapping(uint256 clientId => Client client) private clients;
    mapping(address account => uint256 clientId) private clientIdByAddress;
    address[] private allClientAddresses;
    mapping(address account => uint256 indexPlusOne) private allAddressIndex;

    event Initialized(address indexed owner, uint64 maxTotalReservedGas, uint64 maxClientReservedGas);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);
    event LimitsUpdated(uint64 maxTotalReservedGas, uint64 maxClientReservedGas);
    event ClientCreated(uint256 indexed clientId, address indexed admin, uint64 gasQuota, string metadata);
    event ClientAdminUpdated(uint256 indexed clientId, address indexed previousAdmin, address indexed newAdmin);
    event ClientQuotaUpdated(uint256 indexed clientId, uint64 previousQuota, uint64 newQuota);
    event ClientActiveUpdated(uint256 indexed clientId, bool active);
    event ClientMetadataUpdated(uint256 indexed clientId, string metadata);
    event ClientAddressAdded(uint256 indexed clientId, address indexed account);
    event ClientAddressRemoved(uint256 indexed clientId, address indexed account);
    event ClientFeeModeUpdated(uint256 indexed clientId, uint8 feeMode);
    event ClientEffectiveFromUpdated(uint256 indexed clientId, uint64 effectiveFrom);
    event ConfigVersionUpdated(uint256 version);

    modifier onlyInitialized() {
        if (owner == address(0)) revert NotInitialized();
        _;
    }

    // bumpsVersion increments configVersion after a mutation so Bor's cached
    // snapshot (keyed on root()) is invalidated. Applied to every function that
    // changes the reserved set, quotas, fee modes, effective heights, or limits.
    modifier bumpsVersion() {
        _;
        unchecked {
            configVersion++;
        }
        emit ConfigVersionUpdated(configVersion);
    }

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    modifier onlyClientAdminOrOwner(uint256 clientId) {
        Client storage client = _client(clientId);
        if (msg.sender != owner && msg.sender != client.admin) revert NotClientAdmin();
        _;
    }

    function initialize(address initialOwner, uint64 maxTotalGas, uint64 maxClientGas) external {
        if (owner != address(0)) revert AlreadyInitialized();
        if (initialOwner == address(0)) revert ZeroAddress();
        _validateLimits(maxTotalGas, maxClientGas);

        owner = initialOwner;
        maxTotalReservedGas = maxTotalGas;
        maxClientReservedGas = maxClientGas;
        nextClientId = 1;

        emit Initialized(initialOwner, maxTotalGas, maxClientGas);
    }

    function transferOwnership(address newOwner) external onlyInitialized onlyOwner {
        if (newOwner == address(0)) revert ZeroAddress();
        emit OwnershipTransferred(owner, newOwner);
        owner = newOwner;
    }

    function setLimits(uint64 maxTotalGas, uint64 maxClientGas) external onlyInitialized onlyOwner bumpsVersion {
        _validateLimits(maxTotalGas, maxClientGas);
        if (totalReservedGas > maxTotalGas) revert MaxTotalReservedGasExceeded();
        for (uint256 clientId = 1; clientId < nextClientId; clientId++) {
            if (clients[clientId].gasQuota > maxClientGas) revert MaxClientReservedGasExceeded();
        }

        maxTotalReservedGas = maxTotalGas;
        maxClientReservedGas = maxClientGas;

        emit LimitsUpdated(maxTotalGas, maxClientGas);
    }

    function createClient(
        address admin,
        uint64 gasQuota,
        uint8 feeMode,
        uint64 effectiveFrom,
        string calldata metadata,
        address[] calldata addresses
    ) external onlyInitialized onlyOwner bumpsVersion returns (uint256 clientId) {
        if (admin == address(0)) revert ZeroAddress();
        if (feeMode > FEE_MODE_ROUTED) revert InvalidFeeMode();
        _validateQuota(gasQuota, 0);

        clientId = nextClientId++;
        Client storage client = clients[clientId];
        client.admin = admin;
        client.gasQuota = gasQuota;
        client.active = true;
        client.feeMode = feeMode;
        client.effectiveFrom = effectiveFrom;
        client.metadata = metadata;
        totalReservedGas += gasQuota;

        for (uint256 i = 0; i < addresses.length; i++) {
            _addAddress(clientId, addresses[i]);
        }

        emit ClientCreated(clientId, admin, gasQuota, metadata);
        emit ClientFeeModeUpdated(clientId, feeMode);
        emit ClientEffectiveFromUpdated(clientId, effectiveFrom);
    }

    function setClientAdmin(uint256 clientId, address newAdmin) external onlyInitialized onlyOwner {
        if (newAdmin == address(0)) revert ZeroAddress();
        Client storage client = _client(clientId);
        address previousAdmin = client.admin;
        client.admin = newAdmin;

        emit ClientAdminUpdated(clientId, previousAdmin, newAdmin);
    }

    function setClientQuota(uint256 clientId, uint64 newQuota) external onlyInitialized onlyOwner bumpsVersion {
        Client storage client = _client(clientId);
        uint64 previousQuota = client.gasQuota;
        _validateQuota(newQuota, client.active ? previousQuota : 0);

        if (client.active) {
            totalReservedGas = totalReservedGas - previousQuota + newQuota;
        }
        client.gasQuota = newQuota;

        emit ClientQuotaUpdated(clientId, previousQuota, newQuota);
    }

    function setClientActive(uint256 clientId, bool active) external onlyInitialized onlyOwner bumpsVersion {
        Client storage client = _client(clientId);
        if (client.active == active) return;

        if (active) {
            _validateQuota(client.gasQuota, 0);
            totalReservedGas += client.gasQuota;
        } else {
            totalReservedGas -= client.gasQuota;
        }
        client.active = active;

        emit ClientActiveUpdated(clientId, active);
    }

    function setClientMetadata(uint256 clientId, string calldata metadata)
        external
        onlyInitialized
        onlyClientAdminOrOwner(clientId)
    {
        clients[clientId].metadata = metadata;
        emit ClientMetadataUpdated(clientId, metadata);
    }

    function addClientAddress(uint256 clientId, address account)
        external
        onlyInitialized
        onlyClientAdminOrOwner(clientId)
        bumpsVersion
    {
        _addAddress(clientId, account);
    }

    function addClientAddresses(uint256 clientId, address[] calldata accounts)
        external
        onlyInitialized
        onlyClientAdminOrOwner(clientId)
        bumpsVersion
    {
        for (uint256 i = 0; i < accounts.length; i++) {
            _addAddress(clientId, accounts[i]);
        }
    }

    function removeClientAddress(uint256 clientId, address account)
        external
        onlyInitialized
        onlyClientAdminOrOwner(clientId)
        bumpsVersion
    {
        Client storage client = _client(clientId);
        if (clientIdByAddress[account] != clientId) revert AddressNotRegistered();

        delete clientIdByAddress[account];
        _removeFromClient(client, account);
        _removeFromGlobal(account);

        emit ClientAddressRemoved(clientId, account);
    }

    function clientCount() external view returns (uint256) {
        if (nextClientId == 0) return 0;
        return nextClientId - 1;
    }

    function getClient(uint256 clientId)
        external
        view
        returns (address admin, uint64 gasQuota, bool active, string memory metadata, uint256 addressCount)
    {
        Client storage client = _client(clientId);
        return (client.admin, client.gasQuota, client.active, client.metadata, client.addresses.length);
    }

    function getClientAddresses(uint256 clientId) external view returns (address[] memory) {
        return _client(clientId).addresses;
    }

    function getClientId(address account) external view returns (uint256) {
        return clientIdByAddress[account];
    }

    function getClientForAddress(address account)
        external
        view
        returns (uint256 clientId, uint64 gasQuota, address admin, bool active, uint8 feeMode, uint64 effectiveFrom)
    {
        clientId = clientIdByAddress[account];
        if (clientId == 0) return (0, 0, address(0), false, 0, 0);

        Client storage client = clients[clientId];
        return (clientId, client.gasQuota, client.admin, client.active, client.feeMode, client.effectiveFrom);
    }

    function isReservedAddress(address account) external view returns (bool) {
        uint256 clientId = clientIdByAddress[account];
        return clientId != 0 && clients[clientId].active;
    }

    // root returns a value that changes whenever the reserved set, quotas, fee
    // modes, effective heights, or limits change. Bor caches its snapshot keyed
    // on this and only rebuilds when it moves.
    function root() external view returns (bytes32) {
        return bytes32(configVersion);
    }

    function setClientFeeMode(uint256 clientId, uint8 feeMode) external onlyInitialized onlyOwner bumpsVersion {
        if (feeMode > FEE_MODE_ROUTED) revert InvalidFeeMode();
        _client(clientId).feeMode = feeMode;
        emit ClientFeeModeUpdated(clientId, feeMode);
    }

    function setClientEffectiveFrom(uint256 clientId, uint64 effectiveFrom)
        external
        onlyInitialized
        onlyOwner
        bumpsVersion
    {
        _client(clientId).effectiveFrom = effectiveFrom;
        emit ClientEffectiveFromUpdated(clientId, effectiveFrom);
    }

    function getWhitelistedAddresses() external view returns (address[] memory) {
        uint256 count;
        for (uint256 i = 0; i < allClientAddresses.length; i++) {
            if (clients[clientIdByAddress[allClientAddresses[i]]].active) count++;
        }

        address[] memory activeAddresses = new address[](count);
        uint256 out;
        for (uint256 i = 0; i < allClientAddresses.length; i++) {
            address account = allClientAddresses[i];
            if (clients[clientIdByAddress[account]].active) {
                activeAddresses[out++] = account;
            }
        }
        return activeAddresses;
    }

    function getReservedClients()
        external
        view
        returns (uint256[] memory clientIds, address[] memory admins, uint64[] memory gasQuotas)
    {
        uint256 count;
        for (uint256 clientId = 1; clientId < nextClientId; clientId++) {
            if (clients[clientId].active) count++;
        }

        clientIds = new uint256[](count);
        admins = new address[](count);
        gasQuotas = new uint64[](count);

        uint256 out;
        for (uint256 clientId = 1; clientId < nextClientId; clientId++) {
            Client storage client = clients[clientId];
            if (!client.active) continue;
            clientIds[out] = clientId;
            admins[out] = client.admin;
            gasQuotas[out] = client.gasQuota;
            out++;
        }
    }

    function _client(uint256 clientId) private view returns (Client storage client) {
        client = clients[clientId];
        if (client.admin == address(0)) revert UnknownClient();
    }

    function _addAddress(uint256 clientId, address account) private {
        if (account == address(0)) revert ZeroAddress();
        if (clientIdByAddress[account] != 0) revert AddressAlreadyRegistered();

        Client storage client = _client(clientId);
        client.addresses.push(account);
        clientIdByAddress[account] = clientId;
        allAddressIndex[account] = allClientAddresses.length + 1;
        allClientAddresses.push(account);

        emit ClientAddressAdded(clientId, account);
    }

    function _removeFromClient(Client storage client, address account) private {
        uint256 length = client.addresses.length;
        for (uint256 i = 0; i < length; i++) {
            if (client.addresses[i] == account) {
                if (i != length - 1) {
                    client.addresses[i] = client.addresses[length - 1];
                }
                client.addresses.pop();
                return;
            }
        }
        revert AddressNotRegistered();
    }

    function _removeFromGlobal(address account) private {
        uint256 indexPlusOne = allAddressIndex[account];
        if (indexPlusOne == 0) revert AddressNotRegistered();

        uint256 index = indexPlusOne - 1;
        uint256 lastIndex = allClientAddresses.length - 1;
        if (index != lastIndex) {
            address moved = allClientAddresses[lastIndex];
            allClientAddresses[index] = moved;
            allAddressIndex[moved] = index + 1;
        }
        allClientAddresses.pop();
        delete allAddressIndex[account];
    }

    function _validateLimits(uint64 maxTotalGas, uint64 maxClientGas) private pure {
        if (maxTotalGas == 0 || maxClientGas == 0 || maxClientGas > maxTotalGas) revert InvalidQuota();
    }

    function _validateQuota(uint64 newQuota, uint64 previousActiveQuota) private view {
        if (newQuota == 0) revert InvalidQuota();
        if (newQuota > maxClientReservedGas) revert MaxClientReservedGasExceeded();
        if (totalReservedGas - previousActiveQuota + newQuota > maxTotalReservedGas) {
            revert MaxTotalReservedGasExceeded();
        }
    }
}
