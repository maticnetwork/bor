package prand

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"testing"
	"testing/iotest"
)

// mostly copy-paste from golang reposetory
// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

const (
	numTestSamples = 10000
)

type statsResults struct {
	mean        float64
	stddev      float64
	closeEnough float64
	maxError    float64
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func nearEqual(a, b, closeEnough, maxError float64) bool {
	absDiff := math.Abs(a - b)
	if absDiff < closeEnough { // Necessary when one value is zero and one value is close to zero.
		return true
	}
	return absDiff/max(math.Abs(a), math.Abs(b)) < maxError
}

var testSeeds = []int64{1, 1754801282, 1698661970, 1550503961}

// checkSimilarDistribution returns success if the mean and stddev of the
// two statsResults are similar.
func (this *statsResults) checkSimilarDistribution(expected *statsResults) error {
	if !nearEqual(this.mean, expected.mean, expected.closeEnough, expected.maxError) {
		s := fmt.Sprintf("mean %v != %v (allowed error %v, %v)", this.mean, expected.mean, expected.closeEnough, expected.maxError)
		fmt.Println(s)
		return errors.New(s)
	}
	if !nearEqual(this.stddev, expected.stddev, expected.closeEnough, expected.maxError) {
		s := fmt.Sprintf("stddev %v != %v (allowed error %v, %v)", this.stddev, expected.stddev, expected.closeEnough, expected.maxError)
		fmt.Println(s)
		return errors.New(s)
	}
	return nil
}

func getStatsResults(samples []float64) *statsResults {
	res := new(statsResults)
	var sum, squaresum float64
	for _, s := range samples {
		sum += s
		squaresum += s * s
	}
	res.mean = sum / float64(len(samples))
	res.stddev = math.Sqrt(squaresum/float64(len(samples)) - res.mean*res.mean)
	return res
}

func checkSampleDistribution(t *testing.T, samples []float64, expected *statsResults) {
	t.Helper()
	actual := getStatsResults(samples)
	err := actual.checkSimilarDistribution(expected)
	if err != nil {
		t.Errorf(err.Error())
	}
}

func checkSampleSliceDistributions(t *testing.T, samples []float64, nslices int, expected *statsResults) {
	t.Helper()
	chunk := len(samples) / nslices
	for i := 0; i < nslices; i++ {
		low := i * chunk
		var high int
		if i == nslices-1 {
			high = len(samples) - 1
		} else {
			high = (i + 1) * chunk
		}
		checkSampleDistribution(t, samples[low:high], expected)
	}
}

//
// Normal distribution tests
//

func generateNormalSamples(nsamples int, mean, stddev float64, seed int64) []float64 {
	r := NewRand()
	samples := make([]float64, nsamples)
	for i := range samples {
		samples[i] = r.NormFloat64()*stddev + mean
	}
	return samples
}

func testNormalDistribution(t *testing.T, nsamples int, mean, stddev float64, seed int64) {
	//fmt.Printf("testing nsamples=%v mean=%v stddev=%v seed=%v\n", nsamples, mean, stddev, seed);

	samples := generateNormalSamples(nsamples, mean, stddev, seed)
	errorScale := max(1.0, stddev) // Error scales with stddev
	expected := &statsResults{mean, stddev, 0.10 * errorScale, 0.08 * errorScale}

	// Make sure that the entire set matches the expected distribution.
	checkSampleDistribution(t, samples, expected)

	// Make sure that each half of the set matches the expected distribution.
	checkSampleSliceDistributions(t, samples, 2, expected)

	// Make sure that each 7th of the set matches the expected distribution.
	checkSampleSliceDistributions(t, samples, 7, expected)
}

// Actual tests

func TestStandardNormalValues(t *testing.T) {
	for _, seed := range testSeeds {
		testNormalDistribution(t, numTestSamples, 0, 1, seed)
	}
}

func TestNonStandardNormalValues(t *testing.T) {
	sdmax := 1000.0
	mmax := 1000.0
	if testing.Short() {
		sdmax = 5
		mmax = 5
	}
	for sd := 0.5; sd < sdmax; sd *= 2 {
		for m := 0.5; m < mmax; m *= 2 {
			for _, seed := range testSeeds {
				testNormalDistribution(t, numTestSamples, m, sd, seed)
				if testing.Short() {
					break
				}
			}
		}
	}
}

//
// Exponential distribution tests
//

func generateExponentialSamples(nsamples int, rate float64, seed int64) []float64 {
	r := NewRand()
	samples := make([]float64, nsamples)
	for i := range samples {
		samples[i] = r.ExpFloat64() / rate
	}
	return samples
}

func testExponentialDistribution(t *testing.T, nsamples int, rate float64, seed int64) {
	//fmt.Printf("testing nsamples=%v rate=%v seed=%v\n", nsamples, rate, seed);

	mean := 1 / rate
	stddev := mean

	samples := generateExponentialSamples(nsamples, rate, seed)
	errorScale := max(1.0, 1/rate) // Error scales with the inverse of the rate
	expected := &statsResults{mean, stddev, 0.10 * errorScale, 0.20 * errorScale}

	// Make sure that the entire set matches the expected distribution.
	checkSampleDistribution(t, samples, expected)

	// Make sure that each half of the set matches the expected distribution.
	checkSampleSliceDistributions(t, samples, 2, expected)

	// Make sure that each 7th of the set matches the expected distribution.
	checkSampleSliceDistributions(t, samples, 7, expected)
}

// Actual tests

func TestStandardExponentialValues(t *testing.T) {
	for _, seed := range testSeeds {
		testExponentialDistribution(t, numTestSamples, 1, seed)
	}
}

func TestNonStandardExponentialValues(t *testing.T) {
	for rate := 0.05; rate < 10; rate *= 2 {
		for _, seed := range testSeeds {
			testExponentialDistribution(t, numTestSamples, rate, seed)
			if testing.Short() {
				break
			}
		}
	}
}

//
// Table generation tests
//

func initNorm() (testKn []uint32, testWn, testFn []float32) {
	const m1 = 1 << 31
	var (
		dn float64 = rn
		tn         = dn
		vn float64 = 9.91256303526217e-3
	)

	testKn = make([]uint32, 128)
	testWn = make([]float32, 128)
	testFn = make([]float32, 128)

	q := vn / math.Exp(-0.5*dn*dn)
	testKn[0] = uint32((dn / q) * m1)
	testKn[1] = 0
	testWn[0] = float32(q / m1)
	testWn[127] = float32(dn / m1)
	testFn[0] = 1.0
	testFn[127] = float32(math.Exp(-0.5 * dn * dn))
	for i := 126; i >= 1; i-- {
		dn = math.Sqrt(-2.0 * math.Log(vn/dn+math.Exp(-0.5*dn*dn)))
		testKn[i+1] = uint32((dn / tn) * m1)
		tn = dn
		testFn[i] = float32(math.Exp(-0.5 * dn * dn))
		testWn[i] = float32(dn / m1)
	}
	return
}

func initExp() (testKe []uint32, testWe, testFe []float32) {
	const m2 = 1 << 32
	var (
		de float64 = re
		te         = de
		ve float64 = 3.9496598225815571993e-3
	)

	testKe = make([]uint32, 256)
	testWe = make([]float32, 256)
	testFe = make([]float32, 256)

	q := ve / math.Exp(-de)
	testKe[0] = uint32((de / q) * m2)
	testKe[1] = 0
	testWe[0] = float32(q / m2)
	testWe[255] = float32(de / m2)
	testFe[0] = 1.0
	testFe[255] = float32(math.Exp(-de))
	for i := 254; i >= 1; i-- {
		de = -math.Log(ve/de + math.Exp(-de))
		testKe[i+1] = uint32((de / te) * m2)
		te = de
		testFe[i] = float32(math.Exp(-de))
		testWe[i] = float32(de / m2)
	}
	return
}

// compareUint32Slices returns the first index where the two slices
// disagree, or <0 if the lengths are the same and all elements
// are identical.
func compareUint32Slices(s1, s2 []uint32) int {
	if len(s1) != len(s2) {
		if len(s1) > len(s2) {
			return len(s2) + 1
		}
		return len(s1) + 1
	}
	for i := range s1 {
		if s1[i] != s2[i] {
			return i
		}
	}
	return -1
}

// compareFloat32Slices returns the first index where the two slices
// disagree, or <0 if the lengths are the same and all elements
// are identical.
func compareFloat32Slices(s1, s2 []float32) int {
	if len(s1) != len(s2) {
		if len(s1) > len(s2) {
			return len(s2) + 1
		}
		return len(s1) + 1
	}
	for i := range s1 {
		if !nearEqual(float64(s1[i]), float64(s2[i]), 0, 1e-7) {
			return i
		}
	}
	return -1
}

func TestNormTables(t *testing.T) {
	testKn, testWn, testFn := initNorm()
	if i := compareUint32Slices(kn[0:], testKn); i >= 0 {
		t.Errorf("kn disagrees at index %v; %v != %v", i, kn[i], testKn[i])
	}
	if i := compareFloat32Slices(wn[0:], testWn); i >= 0 {
		t.Errorf("wn disagrees at index %v; %v != %v", i, wn[i], testWn[i])
	}
	if i := compareFloat32Slices(fn[0:], testFn); i >= 0 {
		t.Errorf("fn disagrees at index %v; %v != %v", i, fn[i], testFn[i])
	}
}

func TestExpTables(t *testing.T) {
	testKe, testWe, testFe := initExp()
	if i := compareUint32Slices(ke[0:], testKe); i >= 0 {
		t.Errorf("ke disagrees at index %v; %v != %v", i, ke[i], testKe[i])
	}
	if i := compareFloat32Slices(we[0:], testWe); i >= 0 {
		t.Errorf("we disagrees at index %v; %v != %v", i, we[i], testWe[i])
	}
	if i := compareFloat32Slices(fe[0:], testFe); i >= 0 {
		t.Errorf("fe disagrees at index %v; %v != %v", i, fe[i], testFe[i])
	}
}

func hasSlowFloatingPoint() bool {
	switch runtime.GOARCH {
	case "arm":
		return os.Getenv("GOARM") == "5"
	case "mips", "mipsle", "mips64", "mips64le":
		// Be conservative and assume that all mips boards
		// have emulated floating point.
		// TODO: detect what it actually has.
		return true
	}
	return false
}

func TestFloat32(t *testing.T) {
	// For issue 6721, the problem came after 7533753 calls, so check 10e6.
	num := int(10e6)
	// But do the full amount only on builders (not locally).
	// But ARM5 floating point emulation is slow (Issue 10749), so
	// do less for that builder:
	if testing.Short() {
		num /= 100 // 1.72 seconds instead of 172 seconds
	}

	r := NewRand()
	for ct := 0; ct < num; ct++ {
		f := r.Float32()
		if f >= 1 {
			t.Fatal("Float32() should be in range [0,1). ct:", ct, "f:", f)
		}
	}
}

func testReadUniformity(t *testing.T, n int, seed int64) {
	r := NewRand()
	buf := make([]byte, n)
	nRead, err := r.Read(buf)
	if err != nil {
		t.Errorf("Read err %v", err)
	}
	if nRead != n {
		t.Errorf("Read returned unexpected n; %d != %d", nRead, n)
	}

	// Expect a uniform distribution of byte values, which lie in [0, 255].
	var (
		mean       = 255.0 / 2
		stddev     = 256.0 / math.Sqrt(12.0)
		errorScale = stddev / math.Sqrt(float64(n))
	)

	expected := &statsResults{mean, stddev, 0.10 * errorScale, 0.08 * errorScale}

	// Cast bytes as floats to use the common distribution-validity checks.
	samples := make([]float64, n)
	for i, val := range buf {
		samples[i] = float64(val)
	}
	// Make sure that the entire set matches the expected distribution.
	checkSampleDistribution(t, samples, expected)
}

func TestReadUniformity(t *testing.T) {
	testBufferSizes := []int{
		2, 4, 7, 64, 1024, 1 << 16, 1 << 20,
	}
	for _, seed := range testSeeds {
		for _, n := range testBufferSizes {
			testReadUniformity(t, n, seed)
		}
	}
}

func TestReadEmpty(t *testing.T) {
	r := NewRand()
	buf := make([]byte, 0)
	n, err := r.Read(buf)
	if err != nil {
		t.Errorf("Read err into empty buffer; %v", err)
	}
	if n != 0 {
		t.Errorf("Read into empty buffer returned unexpected n of %d", n)
	}
}

func TestReadByOneByte(t *testing.T) {
	r := NewRand()
	b1 := make([]byte, 100)
	_, err := io.ReadFull(iotest.OneByteReader(r), b1)
	if err != nil {
		t.Errorf("read by one byte: %v", err)
	}
	r = NewRand()
	b2 := make([]byte, 100)
	_, err = r.Read(b2)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	if bytes.Equal(b1, b2) {
		t.Errorf("read by one byte vs single read:\n%x\n%x", b1, b2)
	}
}

func TestShuffleSmall(t *testing.T) {
	// Check that Shuffle allows n=0 and n=1, but that swap is never called for them.
	r := NewRand()
	for n := 0; n <= 1; n++ {
		r.Shuffle(n, func(i, j int) { t.Fatalf("swap called, n=%d i=%d j=%d", n, i, j) })
	}
}

var kn = [128]uint32{
	0x76ad2212, 0x0, 0x600f1b53, 0x6ce447a6, 0x725b46a2,
	0x7560051d, 0x774921eb, 0x789a25bd, 0x799045c3, 0x7a4bce5d,
	0x7adf629f, 0x7b5682a6, 0x7bb8a8c6, 0x7c0ae722, 0x7c50cce7,
	0x7c8cec5b, 0x7cc12cd6, 0x7ceefed2, 0x7d177e0b, 0x7d3b8883,
	0x7d5bce6c, 0x7d78dd64, 0x7d932886, 0x7dab0e57, 0x7dc0dd30,
	0x7dd4d688, 0x7de73185, 0x7df81cea, 0x7e07c0a3, 0x7e163efa,
	0x7e23b587, 0x7e303dfd, 0x7e3beec2, 0x7e46db77, 0x7e51155d,
	0x7e5aabb3, 0x7e63abf7, 0x7e6c222c, 0x7e741906, 0x7e7b9a18,
	0x7e82adfa, 0x7e895c63, 0x7e8fac4b, 0x7e95a3fb, 0x7e9b4924,
	0x7ea0a0ef, 0x7ea5b00d, 0x7eaa7ac3, 0x7eaf04f3, 0x7eb3522a,
	0x7eb765a5, 0x7ebb4259, 0x7ebeeafd, 0x7ec2620a, 0x7ec5a9c4,
	0x7ec8c441, 0x7ecbb365, 0x7ece78ed, 0x7ed11671, 0x7ed38d62,
	0x7ed5df12, 0x7ed80cb4, 0x7eda175c, 0x7edc0005, 0x7eddc78e,
	0x7edf6ebf, 0x7ee0f647, 0x7ee25ebe, 0x7ee3a8a9, 0x7ee4d473,
	0x7ee5e276, 0x7ee6d2f5, 0x7ee7a620, 0x7ee85c10, 0x7ee8f4cd,
	0x7ee97047, 0x7ee9ce59, 0x7eea0eca, 0x7eea3147, 0x7eea3568,
	0x7eea1aab, 0x7ee9e071, 0x7ee98602, 0x7ee90a88, 0x7ee86d08,
	0x7ee7ac6a, 0x7ee6c769, 0x7ee5bc9c, 0x7ee48a67, 0x7ee32efc,
	0x7ee1a857, 0x7edff42f, 0x7ede0ffa, 0x7edbf8d9, 0x7ed9ab94,
	0x7ed7248d, 0x7ed45fae, 0x7ed1585c, 0x7ece095f, 0x7eca6ccb,
	0x7ec67be2, 0x7ec22eee, 0x7ebd7d1a, 0x7eb85c35, 0x7eb2c075,
	0x7eac9c20, 0x7ea5df27, 0x7e9e769f, 0x7e964c16, 0x7e8d44ba,
	0x7e834033, 0x7e781728, 0x7e6b9933, 0x7e5d8a1a, 0x7e4d9ded,
	0x7e3b737a, 0x7e268c2f, 0x7e0e3ff5, 0x7df1aa5d, 0x7dcf8c72,
	0x7da61a1e, 0x7d72a0fb, 0x7d30e097, 0x7cd9b4ab, 0x7c600f1a,
	0x7ba90bdc, 0x7a722176, 0x77d664e5,
}
var wn = [128]float32{
	1.7290405e-09, 1.2680929e-10, 1.6897518e-10, 1.9862688e-10,
	2.2232431e-10, 2.4244937e-10, 2.601613e-10, 2.7611988e-10,
	2.9073963e-10, 3.042997e-10, 3.1699796e-10, 3.289802e-10,
	3.4035738e-10, 3.5121603e-10, 3.616251e-10, 3.7164058e-10,
	3.8130857e-10, 3.9066758e-10, 3.9975012e-10, 4.08584e-10,
	4.1719309e-10, 4.2559822e-10, 4.338176e-10, 4.418672e-10,
	4.497613e-10, 4.5751258e-10, 4.651324e-10, 4.7263105e-10,
	4.8001775e-10, 4.87301e-10, 4.944885e-10, 5.015873e-10,
	5.0860405e-10, 5.155446e-10, 5.2241467e-10, 5.2921934e-10,
	5.359635e-10, 5.426517e-10, 5.4928817e-10, 5.5587696e-10,
	5.624219e-10, 5.6892646e-10, 5.753941e-10, 5.818282e-10,
	5.882317e-10, 5.946077e-10, 6.00959e-10, 6.072884e-10,
	6.135985e-10, 6.19892e-10, 6.2617134e-10, 6.3243905e-10,
	6.386974e-10, 6.449488e-10, 6.511956e-10, 6.5744005e-10,
	6.6368433e-10, 6.699307e-10, 6.7618144e-10, 6.824387e-10,
	6.8870465e-10, 6.949815e-10, 7.012715e-10, 7.075768e-10,
	7.1389966e-10, 7.202424e-10, 7.266073e-10, 7.329966e-10,
	7.394128e-10, 7.4585826e-10, 7.5233547e-10, 7.58847e-10,
	7.653954e-10, 7.719835e-10, 7.7861395e-10, 7.852897e-10,
	7.920138e-10, 7.987892e-10, 8.0561924e-10, 8.125073e-10,
	8.194569e-10, 8.2647167e-10, 8.3355556e-10, 8.407127e-10,
	8.479473e-10, 8.55264e-10, 8.6266755e-10, 8.7016316e-10,
	8.777562e-10, 8.8545243e-10, 8.932582e-10, 9.0117996e-10,
	9.09225e-10, 9.174008e-10, 9.2571584e-10, 9.341788e-10,
	9.427997e-10, 9.515889e-10, 9.605579e-10, 9.697193e-10,
	9.790869e-10, 9.88676e-10, 9.985036e-10, 1.0085882e-09,
	1.0189509e-09, 1.0296151e-09, 1.0406069e-09, 1.0519566e-09,
	1.063698e-09, 1.0758702e-09, 1.0885183e-09, 1.1016947e-09,
	1.1154611e-09, 1.1298902e-09, 1.1450696e-09, 1.1611052e-09,
	1.1781276e-09, 1.1962995e-09, 1.2158287e-09, 1.2369856e-09,
	1.2601323e-09, 1.2857697e-09, 1.3146202e-09, 1.347784e-09,
	1.3870636e-09, 1.4357403e-09, 1.5008659e-09, 1.6030948e-09,
}
var fn = [128]float32{
	1, 0.9635997, 0.9362827, 0.9130436, 0.89228165, 0.87324303,
	0.8555006, 0.8387836, 0.8229072, 0.8077383, 0.793177,
	0.7791461, 0.7655842, 0.7524416, 0.73967725, 0.7272569,
	0.7151515, 0.7033361, 0.69178915, 0.68049186, 0.6694277,
	0.658582, 0.6479418, 0.63749546, 0.6272325, 0.6171434,
	0.6072195, 0.5974532, 0.58783704, 0.5783647, 0.56903,
	0.5598274, 0.5507518, 0.54179835, 0.5329627, 0.52424055,
	0.5156282, 0.50712204, 0.49871865, 0.49041483, 0.48220766,
	0.4740943, 0.46607214, 0.4581387, 0.45029163, 0.44252872,
	0.43484783, 0.427247, 0.41972435, 0.41227803, 0.40490642,
	0.39760786, 0.3903808, 0.3832238, 0.37613547, 0.36911446,
	0.3621595, 0.35526937, 0.34844297, 0.34167916, 0.33497685,
	0.3283351, 0.3217529, 0.3152294, 0.30876362, 0.30235484,
	0.29600215, 0.28970486, 0.2834622, 0.2772735, 0.27113807,
	0.2650553, 0.25902456, 0.2530453, 0.24711695, 0.241239,
	0.23541094, 0.22963232, 0.2239027, 0.21822165, 0.21258877,
	0.20700371, 0.20146611, 0.19597565, 0.19053204, 0.18513499,
	0.17978427, 0.17447963, 0.1692209, 0.16400786, 0.15884037,
	0.15371831, 0.14864157, 0.14361008, 0.13862377, 0.13368265,
	0.12878671, 0.12393598, 0.119130544, 0.11437051, 0.10965602,
	0.104987256, 0.10036444, 0.095787846, 0.0912578, 0.08677467,
	0.0823389, 0.077950984, 0.073611505, 0.06932112, 0.06508058,
	0.06089077, 0.056752663, 0.0526674, 0.048636295, 0.044660863,
	0.040742867, 0.03688439, 0.033087887, 0.029356318,
	0.025693292, 0.022103304, 0.018592102, 0.015167298,
	0.011839478, 0.008624485, 0.005548995, 0.0026696292,
}

const (
	rn = 3.442619855899
)

const (
	re = 7.69711747013104972
)

var ke = [256]uint32{
	0xe290a139, 0x0, 0x9beadebc, 0xc377ac71, 0xd4ddb990,
	0xde893fb8, 0xe4a8e87c, 0xe8dff16a, 0xebf2deab, 0xee49a6e8,
	0xf0204efd, 0xf19bdb8e, 0xf2d458bb, 0xf3da104b, 0xf4b86d78,
	0xf577ad8a, 0xf61de83d, 0xf6afb784, 0xf730a573, 0xf7a37651,
	0xf80a5bb6, 0xf867189d, 0xf8bb1b4f, 0xf9079062, 0xf94d70ca,
	0xf98d8c7d, 0xf9c8928a, 0xf9ff175b, 0xfa319996, 0xfa6085f8,
	0xfa8c3a62, 0xfab5084e, 0xfadb36c8, 0xfaff0410, 0xfb20a6ea,
	0xfb404fb4, 0xfb5e2951, 0xfb7a59e9, 0xfb95038c, 0xfbae44ba,
	0xfbc638d8, 0xfbdcf892, 0xfbf29a30, 0xfc0731df, 0xfc1ad1ed,
	0xfc2d8b02, 0xfc3f6c4d, 0xfc5083ac, 0xfc60ddd1, 0xfc708662,
	0xfc7f8810, 0xfc8decb4, 0xfc9bbd62, 0xfca9027c, 0xfcb5c3c3,
	0xfcc20864, 0xfccdd70a, 0xfcd935e3, 0xfce42ab0, 0xfceebace,
	0xfcf8eb3b, 0xfd02c0a0, 0xfd0c3f59, 0xfd156b7b, 0xfd1e48d6,
	0xfd26daff, 0xfd2f2552, 0xfd372af7, 0xfd3eeee5, 0xfd4673e7,
	0xfd4dbc9e, 0xfd54cb85, 0xfd5ba2f2, 0xfd62451b, 0xfd68b415,
	0xfd6ef1da, 0xfd750047, 0xfd7ae120, 0xfd809612, 0xfd8620b4,
	0xfd8b8285, 0xfd90bcf5, 0xfd95d15e, 0xfd9ac10b, 0xfd9f8d36,
	0xfda43708, 0xfda8bf9e, 0xfdad2806, 0xfdb17141, 0xfdb59c46,
	0xfdb9a9fd, 0xfdbd9b46, 0xfdc170f6, 0xfdc52bd8, 0xfdc8ccac,
	0xfdcc542d, 0xfdcfc30b, 0xfdd319ef, 0xfdd6597a, 0xfdd98245,
	0xfddc94e5, 0xfddf91e6, 0xfde279ce, 0xfde54d1f, 0xfde80c52,
	0xfdeab7de, 0xfded5034, 0xfdefd5be, 0xfdf248e3, 0xfdf4aa06,
	0xfdf6f984, 0xfdf937b6, 0xfdfb64f4, 0xfdfd818d, 0xfdff8dd0,
	0xfe018a08, 0xfe03767a, 0xfe05536c, 0xfe07211c, 0xfe08dfc9,
	0xfe0a8fab, 0xfe0c30fb, 0xfe0dc3ec, 0xfe0f48b1, 0xfe10bf76,
	0xfe122869, 0xfe1383b4, 0xfe14d17c, 0xfe1611e7, 0xfe174516,
	0xfe186b2a, 0xfe19843e, 0xfe1a9070, 0xfe1b8fd6, 0xfe1c8289,
	0xfe1d689b, 0xfe1e4220, 0xfe1f0f26, 0xfe1fcfbc, 0xfe2083ed,
	0xfe212bc3, 0xfe21c745, 0xfe225678, 0xfe22d95f, 0xfe234ffb,
	0xfe23ba4a, 0xfe241849, 0xfe2469f2, 0xfe24af3c, 0xfe24e81e,
	0xfe25148b, 0xfe253474, 0xfe2547c7, 0xfe254e70, 0xfe25485a,
	0xfe25356a, 0xfe251586, 0xfe24e88f, 0xfe24ae64, 0xfe2466e1,
	0xfe2411df, 0xfe23af34, 0xfe233eb4, 0xfe22c02c, 0xfe22336b,
	0xfe219838, 0xfe20ee58, 0xfe20358c, 0xfe1f6d92, 0xfe1e9621,
	0xfe1daef0, 0xfe1cb7ac, 0xfe1bb002, 0xfe1a9798, 0xfe196e0d,
	0xfe1832fd, 0xfe16e5fe, 0xfe15869d, 0xfe141464, 0xfe128ed3,
	0xfe10f565, 0xfe0f478c, 0xfe0d84b1, 0xfe0bac36, 0xfe09bd73,
	0xfe07b7b5, 0xfe059a40, 0xfe03644c, 0xfe011504, 0xfdfeab88,
	0xfdfc26e9, 0xfdf98629, 0xfdf6c83b, 0xfdf3ec01, 0xfdf0f04a,
	0xfdedd3d1, 0xfdea953d, 0xfde7331e, 0xfde3abe9, 0xfddffdfb,
	0xfddc2791, 0xfdd826cd, 0xfdd3f9a8, 0xfdcf9dfc, 0xfdcb1176,
	0xfdc65198, 0xfdc15bb3, 0xfdbc2ce2, 0xfdb6c206, 0xfdb117be,
	0xfdab2a63, 0xfda4f5fd, 0xfd9e7640, 0xfd97a67a, 0xfd908192,
	0xfd8901f2, 0xfd812182, 0xfd78d98e, 0xfd7022bb, 0xfd66f4ed,
	0xfd5d4732, 0xfd530f9c, 0xfd48432b, 0xfd3cd59a, 0xfd30b936,
	0xfd23dea4, 0xfd16349e, 0xfd07a7a3, 0xfcf8219b, 0xfce7895b,
	0xfcd5c220, 0xfcc2aadb, 0xfcae1d5e, 0xfc97ed4e, 0xfc7fe6d4,
	0xfc65ccf3, 0xfc495762, 0xfc2a2fc8, 0xfc07ee19, 0xfbe213c1,
	0xfbb8051a, 0xfb890078, 0xfb5411a5, 0xfb180005, 0xfad33482,
	0xfa839276, 0xfa263b32, 0xf9b72d1c, 0xf930a1a2, 0xf889f023,
	0xf7b577d2, 0xf69c650c, 0xf51530f0, 0xf2cb0e3c, 0xeeefb15d,
	0xe6da6ecf,
}
var we = [256]float32{
	2.0249555e-09, 1.486674e-11, 2.4409617e-11, 3.1968806e-11,
	3.844677e-11, 4.4228204e-11, 4.9516443e-11, 5.443359e-11,
	5.905944e-11, 6.344942e-11, 6.7643814e-11, 7.1672945e-11,
	7.556032e-11, 7.932458e-11, 8.298079e-11, 8.654132e-11,
	9.0016515e-11, 9.3415074e-11, 9.674443e-11, 1.0001099e-10,
	1.03220314e-10, 1.06377254e-10, 1.09486115e-10, 1.1255068e-10,
	1.1557435e-10, 1.1856015e-10, 1.2151083e-10, 1.2442886e-10,
	1.2731648e-10, 1.3017575e-10, 1.3300853e-10, 1.3581657e-10,
	1.3860142e-10, 1.4136457e-10, 1.4410738e-10, 1.4683108e-10,
	1.4953687e-10, 1.5222583e-10, 1.54899e-10, 1.5755733e-10,
	1.6020171e-10, 1.6283301e-10, 1.6545203e-10, 1.6805951e-10,
	1.7065617e-10, 1.732427e-10, 1.7581973e-10, 1.7838787e-10,
	1.8094774e-10, 1.8349985e-10, 1.8604476e-10, 1.8858298e-10,
	1.9111498e-10, 1.9364126e-10, 1.9616223e-10, 1.9867835e-10,
	2.0119004e-10, 2.0369768e-10, 2.0620168e-10, 2.087024e-10,
	2.1120022e-10, 2.136955e-10, 2.1618855e-10, 2.1867974e-10,
	2.2116936e-10, 2.2365775e-10, 2.261452e-10, 2.2863202e-10,
	2.311185e-10, 2.3360494e-10, 2.360916e-10, 2.3857874e-10,
	2.4106667e-10, 2.4355562e-10, 2.4604588e-10, 2.485377e-10,
	2.5103128e-10, 2.5352695e-10, 2.560249e-10, 2.585254e-10,
	2.6102867e-10, 2.6353494e-10, 2.6604446e-10, 2.6855745e-10,
	2.7107416e-10, 2.7359479e-10, 2.761196e-10, 2.7864877e-10,
	2.8118255e-10, 2.8372119e-10, 2.8626485e-10, 2.888138e-10,
	2.9136826e-10, 2.939284e-10, 2.9649452e-10, 2.9906677e-10,
	3.016454e-10, 3.0423064e-10, 3.0682268e-10, 3.0942177e-10,
	3.1202813e-10, 3.1464195e-10, 3.1726352e-10, 3.19893e-10,
	3.2253064e-10, 3.251767e-10, 3.2783135e-10, 3.3049485e-10,
	3.3316744e-10, 3.3584938e-10, 3.3854083e-10, 3.4124212e-10,
	3.4395342e-10, 3.46675e-10, 3.4940711e-10, 3.5215003e-10,
	3.5490397e-10, 3.5766917e-10, 3.6044595e-10, 3.6323455e-10,
	3.660352e-10, 3.6884823e-10, 3.7167386e-10, 3.745124e-10,
	3.773641e-10, 3.802293e-10, 3.8310827e-10, 3.860013e-10,
	3.8890866e-10, 3.918307e-10, 3.9476775e-10, 3.9772008e-10,
	4.0068804e-10, 4.0367196e-10, 4.0667217e-10, 4.09689e-10,
	4.1272286e-10, 4.1577405e-10, 4.1884296e-10, 4.2192994e-10,
	4.250354e-10, 4.281597e-10, 4.313033e-10, 4.3446652e-10,
	4.3764986e-10, 4.408537e-10, 4.4407847e-10, 4.4732465e-10,
	4.5059267e-10, 4.5388301e-10, 4.571962e-10, 4.6053267e-10,
	4.6389292e-10, 4.6727755e-10, 4.70687e-10, 4.741219e-10,
	4.7758275e-10, 4.810702e-10, 4.845848e-10, 4.8812715e-10,
	4.9169796e-10, 4.9529775e-10, 4.989273e-10, 5.0258725e-10,
	5.0627835e-10, 5.100013e-10, 5.1375687e-10, 5.1754584e-10,
	5.21369e-10, 5.2522725e-10, 5.2912136e-10, 5.330522e-10,
	5.370208e-10, 5.4102806e-10, 5.45075e-10, 5.491625e-10,
	5.532918e-10, 5.5746385e-10, 5.616799e-10, 5.6594107e-10,
	5.7024857e-10, 5.746037e-10, 5.7900773e-10, 5.834621e-10,
	5.8796823e-10, 5.925276e-10, 5.971417e-10, 6.018122e-10,
	6.065408e-10, 6.113292e-10, 6.1617933e-10, 6.2109295e-10,
	6.260722e-10, 6.3111916e-10, 6.3623595e-10, 6.4142497e-10,
	6.4668854e-10, 6.5202926e-10, 6.5744976e-10, 6.6295286e-10,
	6.6854156e-10, 6.742188e-10, 6.79988e-10, 6.858526e-10,
	6.9181616e-10, 6.978826e-10, 7.04056e-10, 7.103407e-10,
	7.167412e-10, 7.2326256e-10, 7.2990985e-10, 7.366886e-10,
	7.4360473e-10, 7.5066453e-10, 7.5787476e-10, 7.6524265e-10,
	7.7277595e-10, 7.80483e-10, 7.883728e-10, 7.9645507e-10,
	8.047402e-10, 8.1323964e-10, 8.219657e-10, 8.309319e-10,
	8.401528e-10, 8.496445e-10, 8.594247e-10, 8.6951274e-10,
	8.799301e-10, 8.9070046e-10, 9.018503e-10, 9.134092e-10,
	9.254101e-10, 9.378904e-10, 9.508923e-10, 9.644638e-10,
	9.786603e-10, 9.935448e-10, 1.0091913e-09, 1.025686e-09,
	1.0431306e-09, 1.0616465e-09, 1.08138e-09, 1.1025096e-09,
	1.1252564e-09, 1.1498986e-09, 1.1767932e-09, 1.206409e-09,
	1.2393786e-09, 1.276585e-09, 1.3193139e-09, 1.3695435e-09,
	1.4305498e-09, 1.508365e-09, 1.6160854e-09, 1.7921248e-09,
}
var fe = [256]float32{
	1, 0.9381437, 0.90046996, 0.87170434, 0.8477855, 0.8269933,
	0.8084217, 0.7915276, 0.77595687, 0.7614634, 0.7478686,
	0.7350381, 0.72286767, 0.71127474, 0.70019263, 0.6895665,
	0.67935055, 0.6695063, 0.66000086, 0.65080583, 0.6418967,
	0.63325197, 0.6248527, 0.6166822, 0.60872537, 0.60096896,
	0.5934009, 0.58601034, 0.5787874, 0.57172304, 0.5648092,
	0.5580383, 0.5514034, 0.5448982, 0.5385169, 0.53225386,
	0.5261042, 0.52006316, 0.5141264, 0.50828975, 0.5025495,
	0.496902, 0.49134386, 0.485872, 0.48048335, 0.4751752,
	0.46994483, 0.46478975, 0.45970762, 0.45469615, 0.44975325,
	0.44487688, 0.44006512, 0.43531612, 0.43062815, 0.42599955,
	0.42142874, 0.4169142, 0.41245446, 0.40804818, 0.403694,
	0.3993907, 0.39513698, 0.39093173, 0.38677382, 0.38266218,
	0.37859577, 0.37457356, 0.37059465, 0.3666581, 0.362763,
	0.35890847, 0.35509375, 0.351318, 0.3475805, 0.34388044,
	0.34021714, 0.3365899, 0.33299807, 0.32944095, 0.32591796,
	0.3224285, 0.3189719, 0.31554767, 0.31215525, 0.30879408,
	0.3054636, 0.3021634, 0.29889292, 0.2956517, 0.29243928,
	0.28925523, 0.28609908, 0.28297043, 0.27986884, 0.27679393,
	0.2737453, 0.2707226, 0.2677254, 0.26475343, 0.26180625,
	0.25888354, 0.25598502, 0.2531103, 0.25025907, 0.24743107,
	0.24462597, 0.24184346, 0.23908329, 0.23634516, 0.23362878,
	0.23093392, 0.2282603, 0.22560766, 0.22297576, 0.22036438,
	0.21777324, 0.21520215, 0.21265087, 0.21011916, 0.20760682,
	0.20511365, 0.20263945, 0.20018397, 0.19774707, 0.19532852,
	0.19292815, 0.19054577, 0.1881812, 0.18583426, 0.18350479,
	0.1811926, 0.17889754, 0.17661946, 0.17435817, 0.17211354,
	0.1698854, 0.16767362, 0.16547804, 0.16329853, 0.16113494,
	0.15898713, 0.15685499, 0.15473837, 0.15263714, 0.15055119,
	0.14848037, 0.14642459, 0.14438373, 0.14235765, 0.14034624,
	0.13834943, 0.13636707, 0.13439907, 0.13244532, 0.13050574,
	0.1285802, 0.12666863, 0.12477092, 0.12288698, 0.12101672,
	0.119160056, 0.1173169, 0.115487166, 0.11367077, 0.11186763,
	0.11007768, 0.10830083, 0.10653701, 0.10478614, 0.10304816,
	0.101323, 0.09961058, 0.09791085, 0.09622374, 0.09454919,
	0.09288713, 0.091237515, 0.08960028, 0.087975375, 0.08636274,
	0.08476233, 0.083174095, 0.081597984, 0.08003395, 0.07848195,
	0.076941945, 0.07541389, 0.07389775, 0.072393484, 0.07090106,
	0.069420435, 0.06795159, 0.066494495, 0.06504912, 0.063615434,
	0.062193416, 0.060783047, 0.059384305, 0.057997175,
	0.05662164, 0.05525769, 0.053905312, 0.052564494, 0.051235236,
	0.049917534, 0.048611384, 0.047316793, 0.046033762, 0.0447623,
	0.043502413, 0.042254124, 0.041017443, 0.039792392,
	0.038578995, 0.037377283, 0.036187284, 0.035009038,
	0.033842582, 0.032687962, 0.031545233, 0.030414443, 0.02929566,
	0.02818895, 0.027094385, 0.026012046, 0.024942026, 0.023884421,
	0.022839336, 0.021806888, 0.020787204, 0.019780423, 0.0187867,
	0.0178062, 0.016839107, 0.015885621, 0.014945968, 0.014020392,
	0.013109165, 0.012212592, 0.011331013, 0.01046481, 0.009614414,
	0.008780315, 0.007963077, 0.0071633533, 0.006381906,
	0.0056196423, 0.0048776558, 0.004157295, 0.0034602648,
	0.0027887989, 0.0021459677, 0.0015362998, 0.0009672693,
	0.00045413437,
}
