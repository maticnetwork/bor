// File: openzeppelin-solidity/contracts/math/SafeMath.sol

pragma solidity ^0.5.2;

/**
 * @title SafeMath
 * @dev Unsigned math operations with safety checks that revert on error
 */
library SafeMath {
    /**
     * @dev Multiplies two unsigned integers, reverts on overflow.
     */
    function mul(uint256 a, uint256 b) internal pure returns (uint256) {
        // Gas optimization: this is cheaper than requiring 'a' not being zero, but the
        // benefit is lost if 'b' is also tested.
        // See: https://github.com/OpenZeppelin/openzeppelin-solidity/pull/522
        if (a == 0) {
            return 0;
        }

        uint256 c = a * b;
        require(c / a == b);

        return c;
    }

    /**
     * @dev Integer division of two unsigned integers truncating the quotient, reverts on division by zero.
     */
    function div(uint256 a, uint256 b) internal pure returns (uint256) {
        // Solidity only automatically asserts when dividing by 0
        require(b > 0);
        uint256 c = a / b;
        // assert(a == b * c + a % b); // There is no case in which this doesn't hold

        return c;
    }

    /**
     * @dev Subtracts two unsigned integers, reverts on overflow (i.e. if subtrahend is greater than minuend).
     */
    function sub(uint256 a, uint256 b) internal pure returns (uint256) {
        require(b <= a);
        uint256 c = a - b;

        return c;
    }

    /**
     * @dev Adds two unsigned integers, reverts on overflow.
     */
    function add(uint256 a, uint256 b) internal pure returns (uint256) {
        uint256 c = a + b;
        require(c >= a);

        return c;
    }

    /**
     * @dev Divides two unsigned integers and returns the remainder (unsigned integer modulo),
     * reverts when dividing by zero.
     */
    function mod(uint256 a, uint256 b) internal pure returns (uint256) {
        require(b != 0);
        return a % b;
    }
}

// File: solidity-rlp/contracts/RLPReader.sol

/*
* @author Hamdi Allam hamdi.allam97@gmail.com
* Please reach out with any questions or concerns
*/
pragma solidity ^0.5.0;

library RLPReader {
    uint8 constant STRING_SHORT_START = 0x80;
    uint8 constant STRING_LONG_START  = 0xb8;
    uint8 constant LIST_SHORT_START   = 0xc0;
    uint8 constant LIST_LONG_START    = 0xf8;
    uint8 constant WORD_SIZE = 32;

    struct RLPItem {
        uint len;
        uint memPtr;
    }

    struct Iterator {
        RLPItem item;   // Item that's being iterated over.
        uint nextPtr;   // Position of the next item in the list.
    }

    /*
    * @dev Returns the next element in the iteration. Reverts if it has not next element.
    * @param self The iterator.
    * @return The next element in the iteration.
    */
    function next(Iterator memory self) internal pure returns (RLPItem memory) {
        require(hasNext(self));

        uint ptr = self.nextPtr;
        uint itemLength = _itemLength(ptr);
        self.nextPtr = ptr + itemLength;

        return RLPItem(itemLength, ptr);
    }

    /*
    * @dev Returns true if the iteration has more elements.
    * @param self The iterator.
    * @return true if the iteration has more elements.
    */
    function hasNext(Iterator memory self) internal pure returns (bool) {
        RLPItem memory item = self.item;
        return self.nextPtr < item.memPtr + item.len;
    }

    /*
    * @param item RLP encoded bytes
    */
    function toRlpItem(bytes memory item) internal pure returns (RLPItem memory) {
        uint memPtr;
        assembly {
            memPtr := add(item, 0x20)
        }

        return RLPItem(item.length, memPtr);
    }

    /*
    * @dev Create an iterator. Reverts if item is not a list.
    * @param self The RLP item.
    * @return An 'Iterator' over the item.
    */
    function iterator(RLPItem memory self) internal pure returns (Iterator memory) {
        require(isList(self));

        uint ptr = self.memPtr + _payloadOffset(self.memPtr);
        return Iterator(self, ptr);
    }

    /*
    * @param item RLP encoded bytes
    */
    function rlpLen(RLPItem memory item) internal pure returns (uint) {
        return item.len;
    }

    /*
    * @param item RLP encoded bytes
    */
    function payloadLen(RLPItem memory item) internal pure returns (uint) {
        return item.len - _payloadOffset(item.memPtr);
    }

    /*
    * @param item RLP encoded list in bytes
    */
    function toList(RLPItem memory item) internal pure returns (RLPItem[] memory) {
        require(isList(item));

        uint items = numItems(item);
        RLPItem[] memory result = new RLPItem[](items);

        uint memPtr = item.memPtr + _payloadOffset(item.memPtr);
        uint dataLen;
        for (uint i = 0; i < items; i++) {
            dataLen = _itemLength(memPtr);
            result[i] = RLPItem(dataLen, memPtr); 
            memPtr = memPtr + dataLen;
        }

        return result;
    }

    // @return indicator whether encoded payload is a list. negate this function call for isData.
    function isList(RLPItem memory item) internal pure returns (bool) {
        if (item.len == 0) return false;

        uint8 byte0;
        uint memPtr = item.memPtr;
        assembly {
            byte0 := byte(0, mload(memPtr))
        }

        if (byte0 < LIST_SHORT_START)
            return false;
        return true;
    }

    /** RLPItem conversions into data types **/

    // @returns raw rlp encoding in bytes
    function toRlpBytes(RLPItem memory item) internal pure returns (bytes memory) {
        bytes memory result = new bytes(item.len);
        if (result.length == 0) return result;
        
        uint ptr;
        assembly {
            ptr := add(0x20, result)
        }

        copy(item.memPtr, ptr, item.len);
        return result;
    }

    // any non-zero byte is considered true
    function toBoolean(RLPItem memory item) internal pure returns (bool) {
        require(item.len == 1);
        uint result;
        uint memPtr = item.memPtr;
        assembly {
            result := byte(0, mload(memPtr))
        }

        return result == 0 ? false : true;
    }

    function toAddress(RLPItem memory item) internal pure returns (address) {
        // 1 byte for the length prefix
        require(item.len == 21);

        return address(toUint(item));
    }

    function toUint(RLPItem memory item) internal pure returns (uint) {
        require(item.len > 0 && item.len <= 33);

        uint offset = _payloadOffset(item.memPtr);
        uint len = item.len - offset;

        uint result;
        uint memPtr = item.memPtr + offset;
        assembly {
            result := mload(memPtr)

            // shfit to the correct location if neccesary
            if lt(len, 32) {
                result := div(result, exp(256, sub(32, len)))
            }
        }

        return result;
    }

    // enforces 32 byte length
    function toUintStrict(RLPItem memory item) internal pure returns (uint) {
        // one byte prefix
        require(item.len == 33);

        uint result;
        uint memPtr = item.memPtr + 1;
        assembly {
            result := mload(memPtr)
        }

        return result;
    }

    function toBytes(RLPItem memory item) internal pure returns (bytes memory) {
        require(item.len > 0);

        uint offset = _payloadOffset(item.memPtr);
        uint len = item.len - offset; // data length
        bytes memory result = new bytes(len);

        uint destPtr;
        assembly {
            destPtr := add(0x20, result)
        }

        copy(item.memPtr + offset, destPtr, len);
        return result;
    }

    /*
    * Private Helpers
    */

    // @return number of payload items inside an encoded list.
    function numItems(RLPItem memory item) private pure returns (uint) {
        if (item.len == 0) return 0;

        uint count = 0;
        uint currPtr = item.memPtr + _payloadOffset(item.memPtr);
        uint endPtr = item.memPtr + item.len;
        while (currPtr < endPtr) {
           currPtr = currPtr + _itemLength(currPtr); // skip over an item
           count++;
        }

        return count;
    }

    // @return entire rlp item byte length
    function _itemLength(uint memPtr) private pure returns (uint) {
        uint itemLen;
        uint byte0;
        assembly {
            byte0 := byte(0, mload(memPtr))
        }

        if (byte0 < STRING_SHORT_START)
            itemLen = 1;
        
        else if (byte0 < STRING_LONG_START)
            itemLen = byte0 - STRING_SHORT_START + 1;

        else if (byte0 < LIST_SHORT_START) {
            assembly {
                let byteLen := sub(byte0, 0xb7) // # of bytes the actual length is
                memPtr := add(memPtr, 1) // skip over the first byte
                
                /* 32 byte word size */
                let dataLen := div(mload(memPtr), exp(256, sub(32, byteLen))) // right shifting to get the len
                itemLen := add(dataLen, add(byteLen, 1))
            }
        }

        else if (byte0 < LIST_LONG_START) {
            itemLen = byte0 - LIST_SHORT_START + 1;
        } 

        else {
            assembly {
                let byteLen := sub(byte0, 0xf7)
                memPtr := add(memPtr, 1)

                let dataLen := div(mload(memPtr), exp(256, sub(32, byteLen))) // right shifting to the correct length
                itemLen := add(dataLen, add(byteLen, 1))
            }
        }

        return itemLen;
    }

    // @return number of bytes until the data
    function _payloadOffset(uint memPtr) private pure returns (uint) {
        uint byte0;
        assembly {
            byte0 := byte(0, mload(memPtr))
        }

        if (byte0 < STRING_SHORT_START) 
            return 0;
        else if (byte0 < STRING_LONG_START || (byte0 >= LIST_SHORT_START && byte0 < LIST_LONG_START))
            return 1;
        else if (byte0 < LIST_SHORT_START)  // being explicit
            return byte0 - (STRING_LONG_START - 1) + 1;
        else
            return byte0 - (LIST_LONG_START - 1) + 1;
    }

    /*
    * @param src Pointer to source
    * @param dest Pointer to destination
    * @param len Amount of memory to copy from the source
    */
    function copy(uint src, uint dest, uint len) private pure {
        if (len == 0) return;

        // copy as many word sizes as possible
        for (; len >= WORD_SIZE; len -= WORD_SIZE) {
            assembly {
                mstore(dest, mload(src))
            }

            src += WORD_SIZE;
            dest += WORD_SIZE;
        }

        // left over bytes. Mask is used to remove unwanted bytes from the word
        uint mask = 256 ** (WORD_SIZE - len) - 1;
        assembly {
            let srcpart := and(mload(src), not(mask)) // zero out src
            let destpart := and(mload(dest), mask) // retrieve the bytes
            mstore(dest, or(destpart, srcpart))
        }
    }
}

// File: matic-contracts/contracts/common/lib/BytesLib.sol

pragma solidity ^0.5.2;

library BytesLib {
    function concat(bytes memory _preBytes, bytes memory _postBytes)
        internal
        pure
        returns (bytes memory)
    {
        bytes memory tempBytes;
        assembly {
            // Get a location of some free memory and store it in tempBytes as
            // Solidity does for memory variables.
            tempBytes := mload(0x40)

            // Store the length of the first bytes array at the beginning of
            // the memory for tempBytes.
            let length := mload(_preBytes)
            mstore(tempBytes, length)

            // Maintain a memory counter for the current write location in the
            // temp bytes array by adding the 32 bytes for the array length to
            // the starting location.
            let mc := add(tempBytes, 0x20)
            // Stop copying when the memory counter reaches the length of the
            // first bytes array.
            let end := add(mc, length)

            for {
                // Initialize a copy counter to the start of the _preBytes data,
                // 32 bytes into its memory.
                let cc := add(_preBytes, 0x20)
            } lt(mc, end) {
                // Increase both counters by 32 bytes each iteration.
                mc := add(mc, 0x20)
                cc := add(cc, 0x20)
            } {
                // Write the _preBytes data into the tempBytes memory 32 bytes
                // at a time.
                mstore(mc, mload(cc))
            }

            // Add the length of _postBytes to the current length of tempBytes
            // and store it as the new length in the first 32 bytes of the
            // tempBytes memory.
            length := mload(_postBytes)
            mstore(tempBytes, add(length, mload(tempBytes)))

            // Move the memory counter back from a multiple of 0x20 to the
            // actual end of the _preBytes data.
            mc := end
            // Stop copying when the memory counter reaches the new combined
            // length of the arrays.
            end := add(mc, length)

            for {
                let cc := add(_postBytes, 0x20)
            } lt(mc, end) {
                mc := add(mc, 0x20)
                cc := add(cc, 0x20)
            } {
                mstore(mc, mload(cc))
            }

            // Update the free-memory pointer by padding our last write location
            // to 32 bytes: add 31 bytes to the end of tempBytes to move to the
            // next 32 byte block, then round down to the nearest multiple of
            // 32. If the sum of the length of the two arrays is zero then add
            // one before rounding down to leave a blank 32 bytes (the length block with 0).
            mstore(
                0x40,
                and(
                    add(add(end, iszero(add(length, mload(_preBytes)))), 31),
                    not(31) // Round down to the nearest 32 bytes.
                )
            )
        }
        return tempBytes;
    }

    function slice(bytes memory _bytes, uint256 _start, uint256 _length)
        internal
        pure
        returns (bytes memory)
    {
        require(_bytes.length >= (_start + _length));
        bytes memory tempBytes;
        assembly {
            switch iszero(_length)
                case 0 {
                    // Get a location of some free memory and store it in tempBytes as
                    // Solidity does for memory variables.
                    tempBytes := mload(0x40)

                    // The first word of the slice result is potentially a partial
                    // word read from the original array. To read it, we calculate
                    // the length of that partial word and start copying that many
                    // bytes into the array. The first word we copy will start with
                    // data we don't care about, but the last `lengthmod` bytes will
                    // land at the beginning of the contents of the new array. When
                    // we're done copying, we overwrite the full first word with
                    // the actual length of the slice.
                    let lengthmod := and(_length, 31)

                    // The multiplication in the next line is necessary
                    // because when slicing multiples of 32 bytes (lengthmod == 0)
                    // the following copy loop was copying the origin's length
                    // and then ending prematurely not copying everything it should.
                    let mc := add(
                        add(tempBytes, lengthmod),
                        mul(0x20, iszero(lengthmod))
                    )
                    let end := add(mc, _length)

                    for {
                        // The multiplication in the next line has the same exact purpose
                        // as the one above.
                        let cc := add(
                            add(
                                add(_bytes, lengthmod),
                                mul(0x20, iszero(lengthmod))
                            ),
                            _start
                        )
                    } lt(mc, end) {
                        mc := add(mc, 0x20)
                        cc := add(cc, 0x20)
                    } {
                        mstore(mc, mload(cc))
                    }

                    mstore(tempBytes, _length)

                    //update free-memory pointer
                    //allocating the array padded to 32 bytes like the compiler does now
                    mstore(0x40, and(add(mc, 31), not(31)))
                }
                //if we want a zero-length slice let's just return a zero-length array
                default {
                    tempBytes := mload(0x40)
                    mstore(0x40, add(tempBytes, 0x20))
                }
        }

        return tempBytes;
    }

    // Pad a bytes array to 32 bytes
    function leftPad(bytes memory _bytes) internal pure returns (bytes memory) {
        // may underflow if bytes.length < 32. Hence using SafeMath.sub
        bytes memory newBytes = new bytes(SafeMath.sub(32, _bytes.length));
        return concat(newBytes, _bytes);
    }

    function toBytes32(bytes memory b) internal pure returns (bytes32) {
        require(b.length >= 32, "Bytes array should atleast be 32 bytes");
        bytes32 out;
        for (uint256 i = 0; i < 32; i++) {
            out |= bytes32(b[i] & 0xFF) >> (i * 8);
        }
        return out;
    }

    function toBytes4(bytes memory b) internal pure returns (bytes4 result) {
        assembly {
            result := mload(add(b, 32))
        }
    }

    function fromBytes32(bytes32 x) internal pure returns (bytes memory) {
        bytes memory b = new bytes(32);
        for (uint256 i = 0; i < 32; i++) {
            b[i] = bytes1(uint8(uint256(x) / (2**(8 * (31 - i)))));
        }
        return b;
    }

    function fromUint(uint256 _num) internal pure returns (bytes memory _ret) {
        _ret = new bytes(32);
        assembly {
            mstore(add(_ret, 32), _num)
        }
    }

    function toUint(bytes memory _bytes, uint256 _start)
        internal
        pure
        returns (uint256)
    {
        require(_bytes.length >= (_start + 32));
        uint256 tempUint;
        assembly {
            tempUint := mload(add(add(_bytes, 0x20), _start))
        }
        return tempUint;
    }

    function toAddress(bytes memory _bytes, uint256 _start)
        internal
        pure
        returns (address)
    {
        require(_bytes.length >= (_start + 20));
        address tempAddress;
        assembly {
            tempAddress := div(
                mload(add(add(_bytes, 0x20), _start)),
                0x1000000000000000000000000
            )
        }

        return tempAddress;
    }
}

// File: contracts/System.sol

pragma solidity ^0.5.11;

contract System {
  address public constant SYSTEM_ADDRESS = 0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE;

  modifier onlySystem() {
    require(msg.sender == SYSTEM_ADDRESS, "Not System Addess!");
    _;
  }
}

// File: contracts/ECVerify.sol

pragma solidity ^0.5.2;


library ECVerify {
  function ecrecovery(
    bytes32 hash,
    bytes memory sig
  ) internal pure returns (address) {
    bytes32 r;
    bytes32 s;
    uint8 v;

    if (sig.length != 65) {
      return address(0x0);
    }

    assembly {
      r := mload(add(sig, 32))
      s := mload(add(sig, 64))
      v := and(mload(add(sig, 65)), 255)
    }

    // https://github.com/ethereum/go-ethereum/issues/2053
    if (v < 27) {
      v += 27;
    }

    if (v != 27 && v != 28) {
      return address(0x0);
    }

    // get address out of hash and signature
    address result = ecrecover(hash, v, r, s);

    // ecrecover returns zero on error
    require(result != address(0x0));

    return result;
  }

  function ecrecovery(
    bytes32 hash,
    uint8 v,
    bytes32 r,
    bytes32 s
  ) internal pure returns (address) {
    // get address out of hash and signature
    address result = ecrecover(hash, v, r, s);

    // ecrecover returns zero on error
    require(result != address(0x0));

    return result;
  }

  function ecverify(
    bytes32 hash,
    bytes memory sig,
    address signer
  ) internal pure returns (bool) {
    return signer == ecrecovery(hash, sig);
  }
}

// File: contracts/ValidatorSet.sol

pragma solidity ^0.5.11;

interface ValidatorSet {
	// Get initial validator set
	function getInitialValidators()
		external
		view
		returns (address[] memory, uint256[] memory);

	// Get current validator set (last enacted or initial if no changes ever made) with current stake.
	function getValidators()
		external
		view
		returns (address[] memory, uint256[] memory);

	// Check if signer is validator
	function isValidator(uint256 span, address signer)
		external
		view
		returns (bool);

	// Check if signer is producer
	function isProducer(uint256 span, address signer)
		external
		view
		returns (bool);

	// Check if signer is current validator
	function isCurrentValidator(address signer)
		external
		view
		returns (bool);

	// Check if signer is current producer
	function isCurrentProducer(address signer)
		external
		view
		returns (bool);

	// Propose new span
	function proposeSpan()
		external;

	// Pending span proposal
	function spanProposalPending()
		external
		view
		returns (bool);

	// Commit span
	function commitSpan(
		uint256 newSpan,
		uint256 startBlock,
		uint256 endBlock,
		bytes calldata validatorBytes,
		bytes calldata producerBytes
	) external;

	function getSpan(uint256 span)
		external
		view
		returns (uint256 number, uint256 startBlock, uint256 endBlock);

	function getCurrentSpan()
		external
		view 
		returns (uint256 number, uint256 startBlock, uint256 endBlock);	

	function getNextSpan()
		external
		view
		returns (uint256 number, uint256 startBlock, uint256 endBlock);	

	function currentSpanNumber()
		external
		view
		returns (uint256);

	function getSpanByBlock(uint256 number)
		external
		view
		returns (uint256);

	function getBorValidators(uint256 number)
		external
		view
		returns (address[] memory, uint256[] memory);
}

// File: contracts/BorValidatorSet.sol

pragma solidity ^0.5.11;
pragma experimental ABIEncoderV2;





contract BorValidatorSet is System {
  using SafeMath for uint256;
  using RLPReader for bytes;
  using RLPReader for RLPReader.RLPItem;
  using ECVerify for bytes32;

  bytes32 public constant CHAIN = keccak256("heimdall-P5rXwg");
  bytes32 public constant ROUND_TYPE = keccak256("vote");
  bytes32 public constant BOR_ID = keccak256("15001");
  uint8 public constant VOTE_TYPE = 2;
  uint256 public constant FIRST_END_BLOCK = 255;
  uint256 public constant SPRINT = 64;

  struct Validator {
    uint256 id;
    uint256 power;
    address signer;
  }

  // span details
  struct Span {
    uint256 number;
    uint256 startBlock;
    uint256 endBlock;
  }

  mapping(uint256 => Validator[]) public validators;
  mapping(uint256 => Validator[]) public producers;

  mapping (uint256 => Span) public spans; // span number => span
  uint256[] public spanNumbers; // recent span numbers

  // event
  event NewSpan(uint256 indexed id, uint256 indexed startBlock, uint256 indexed endBlock);

  constructor() public {}

  modifier onlyValidator() {
    uint256 span = currentSpanNumber();
    require(isValidator(span, msg.sender));
    _;
  }

  function setInitialValidators() internal {
    address[] memory d;
    uint256[] memory p;

    // get initial validators
    (d, p) = getInitialValidators();

    // initial span
    uint256 span = 0;
    spans[span] = Span({
      number: span,
      startBlock: 0,
      endBlock: FIRST_END_BLOCK
    });
    spanNumbers.push(span);
    validators[span].length = 0;
    producers[span].length = 0;

    for (uint256 i = 0; i < d.length; i++) {
      validators[span].length++;
      validators[span][i] = Validator({
        id: i,
        power: p[i],
        signer: d[i]
      });
    }

    for (uint256 i = 0; i < d.length; i++) {
      producers[span].length++;
      producers[span][i] = Validator({
        id: i,
        power: p[i],
        signer: d[i]
      });
    }
  }

  function currentSprint() public view returns (uint256) {
    return block.number / SPRINT;
  }

  function getSpan(uint256 span) public view returns (uint256 number, uint256 startBlock, uint256 endBlock) {
    return (spans[span].number, spans[span].startBlock, spans[span].endBlock);
  }

  function getCurrentSpan() public view returns (uint256 number, uint256 startBlock, uint256 endBlock) {
    uint256 span = currentSpanNumber();
    return (spans[span].number, spans[span].startBlock, spans[span].endBlock);
  }

  function getNextSpan() public view returns (uint256 number, uint256 startBlock, uint256 endBlock) {
    uint256 span = currentSpanNumber().add(1);
    return (spans[span].number, spans[span].startBlock, spans[span].endBlock);
  }

  function getValidatorsBySpan(uint256 span) internal view returns (Validator[] memory) {
    return validators[span];
  }

  function getProducersBySpan(uint256 span) internal view returns (Validator[] memory) {
    return producers[span];
  }

  // get span number by block
  function getSpanByBlock(uint256 number) public view returns (uint256) {
    for (uint256 i = spanNumbers.length; i > 0; i--) {
      Span memory span = spans[spanNumbers[i - 1]];
      if (span.startBlock <= number && span.endBlock != 0 && number <= span.endBlock) {
        return span.number;
      }
    }

    // if cannot find matching span, return latest span
    if (spanNumbers.length > 0) {
      return spanNumbers[spanNumbers.length - 1];
    }

    // return default if not found any thing
    return 0;
  }

  function currentSpanNumber() public view returns (uint256) {
    return getSpanByBlock(block.number);
  }

  function getValidatorsTotalStakeBySpan(uint256 span) public view returns (uint256) {
    Validator[] memory vals = validators[span];
    uint256 result = 0;
    for (uint256 i = 0; i < vals.length; i++) {
      result = result.add(vals[i].power);
    }
    return result;
  }

  function getProducersTotalStakeBySpan(uint256 span) public view returns (uint256) {
    Validator[] memory vals = producers[span];
    uint256 result = 0;
    for (uint256 i = 0; i < vals.length; i++) {
      result = result.add(vals[i].power);
    }
    return result;
  }

  function getValidatorBySigner(uint256 span, address signer) public view returns (Validator memory result) {
    Validator[] memory vals = validators[span];
    for (uint256 i = 0; i < vals.length; i++) {
      if (vals[i].signer == signer) {
        result = vals[i];
        break;
      }
    }
  }

  function isValidator(uint256 span, address signer) public view returns (bool) {
    Validator[] memory vals = validators[span];
    for (uint256 i = 0; i < vals.length; i++) {
      if (vals[i].signer == signer) {
        return true;
      }
    }
    return false;
  }

  function isProducer(uint256 span, address signer) public view returns (bool) {
    Validator[] memory vals = producers[span];
    for (uint256 i = 0; i < vals.length; i++) {
      if (vals[i].signer == signer) {
        return true;
      }
    }
    return false;
  }

  function isCurrentValidator(address signer) public view returns (bool) {
    return isValidator(currentSpanNumber(), signer);
  }

  function isCurrentProducer(address signer) public view returns (bool) {
    return isProducer(currentSpanNumber(), signer);
  }

  // get bor validator
  function getBorValidators(uint256 number) public view returns (address[] memory, uint256[] memory) {
    if (number <= FIRST_END_BLOCK) {
      return getInitialValidators();
    }

    // span number by block
    uint256 span = getSpanByBlock(number);

    address[] memory addrs = new address[](producers[span].length);
    uint256[] memory powers = new uint256[](producers[span].length);
    for (uint256 i = 0; i < producers[span].length; i++) {
      addrs[i] = producers[span][i].signer;
      powers[i] = producers[span][i].power;
    }

    return (addrs, powers);
  }

  /// Get current validator set (last enacted or initial if no changes ever made) with current stake.
  function getInitialValidators() public view returns (address[] memory, uint256[] memory) {
    address[] memory addrs = new address[](1);
    addrs[0] = 0x6c468CF8c9879006E22EC4029696E005C2319C9D;
    uint256[] memory powers = new uint256[](1);
    powers[0] = 40;
    return (addrs, powers);
  }

  /// Get current validator set (last enacted or initial if no changes ever made) with current stake.
  function getValidators() public view returns (address[] memory, uint256[] memory) {
    return getBorValidators(block.number);
  }

  function commitSpan(
    uint256 newSpan,
    uint256 startBlock,
    uint256 endBlock,
    bytes calldata validatorBytes,
    bytes calldata producerBytes
  ) external onlySystem {
    // current span
    uint256 span = currentSpanNumber();
    // set initial validators if current span is zero
    if (span == 0) {
      setInitialValidators();
    }

    // check conditions
    require(newSpan == span.add(1), "Invalid span id");
    require(endBlock > startBlock, "End block must be greater than start block");
    require((endBlock - startBlock + 1) % SPRINT == 0, "Difference between start and end block must be in multiples of sprint");
    require(spans[span].startBlock <= startBlock, "Start block must be greater than current span");

    // check if already in the span
    require(spans[newSpan].number == 0, "Span already exists");

    // store span
    spans[newSpan] = Span({
      number: newSpan,
      startBlock: startBlock,
      endBlock: endBlock
    });
    spanNumbers.push(newSpan);
    validators[newSpan].length = 0;
    producers[newSpan].length = 0;

    // set validators
    RLPReader.RLPItem[] memory validatorItems = validatorBytes.toRlpItem().toList();
    for (uint256 i = 0; i < validatorItems.length; i++) {
      RLPReader.RLPItem[] memory v = validatorItems[i].toList();
      validators[newSpan].length++;
      validators[newSpan][i] = Validator({
        id: v[0].toUint(),
        power: v[1].toUint(),
        signer: v[2].toAddress()
      });
    }

    // set producers
    RLPReader.RLPItem[] memory producerItems = producerBytes.toRlpItem().toList();
    for (uint256 i = 0; i < producerItems.length; i++) {
      RLPReader.RLPItem[] memory v = producerItems[i].toList();
        producers[newSpan].length++;
        producers[newSpan][i] = Validator({
          id: v[0].toUint(),
          power: v[1].toUint(),
          signer: v[2].toAddress()
        });
    }
  }

  // Get stake power by sigs and data hash
  function getStakePowerBySigs(uint256 span, bytes32 dataHash, bytes memory sigs) public view returns (uint256) {
    uint256 stakePower = 0;
    address lastAdd = address(0x0); // cannot have address(0x0) as an owner

    for (uint64 i = 0; i < sigs.length; i += 65) {
      bytes memory sigElement = BytesLib.slice(sigs, i, 65);
      address signer = dataHash.ecrecovery(sigElement);

      // check if signer is stacker and not proposer
      Validator memory validator = getValidatorBySigner(span, signer);
      if (isValidator(span, signer) && signer > lastAdd) {
        lastAdd = signer;
        stakePower = stakePower.add(validator.power);
      }
    }

    return stakePower;
  }

  //
  // Utility functions
  //

  function checkMembership(
    bytes32 rootHash,
    bytes32 leaf,
    bytes memory proof
  ) public pure returns (bool) {
    bytes32 proofElement;
    byte direction;
    bytes32 computedHash = leaf;

    uint256 len = (proof.length / 33) * 33;
    if (len > 0) {
      computedHash = leafNode(leaf);
    }

    for (uint256 i = 33; i <= len; i += 33) {
      bytes32 tempBytes;
      assembly {
        // Get a location of some free memory and store it in tempBytes as
        // Solidity does for memory variables.
        tempBytes := mload(add(proof, sub(i, 1)))
        proofElement := mload(add(proof, i))
      }

      direction = tempBytes[0];
      if (direction == 0) {
        computedHash = innerNode(proofElement, computedHash);
      } else {
        computedHash = innerNode(computedHash, proofElement);
      }
    }

    return computedHash == rootHash;
  }

  function leafNode(bytes32 d) public pure returns (bytes32) {
    return sha256(abi.encodePacked(byte(uint8(0)), d));
  }

  function innerNode(bytes32 left, bytes32 right) public pure returns (bytes32) {
    return sha256(abi.encodePacked(byte(uint8(1)), left, right));
  }
}
