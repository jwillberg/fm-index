package fmindex

import (
	"bytes"
	"index/suffixarray"
	"testing"
)

var (
	benchmarkSuffixArrayResult []int
	benchmarkStdlibIndexResult *suffixarray.Index
)

func BenchmarkSuffixArrayPrefixDoubling256K(b *testing.B) {
	benchmarkPrefixDoubling(b, 256*1024)
}

func BenchmarkSuffixArrayInternalSAIS256K(b *testing.B) {
	benchmarkInternalSAIS(b, 256*1024)
}

func BenchmarkSuffixArrayStdlibSAIS256K(b *testing.B) {
	benchmarkStdlibSAIS(b, 256*1024)
}

func BenchmarkSuffixArrayPrefixDoubling1M(b *testing.B) {
	benchmarkPrefixDoubling(b, 1024*1024)
}

func BenchmarkSuffixArrayInternalSAIS1M(b *testing.B) {
	benchmarkInternalSAIS(b, 1024*1024)
}

func BenchmarkSuffixArrayStdlibSAIS1M(b *testing.B) {
	benchmarkStdlibSAIS(b, 1024*1024)
}

func BenchmarkSuffixArrayPrefixDoubling4M(b *testing.B) {
	benchmarkPrefixDoubling(b, 4*1024*1024)
}

func BenchmarkSuffixArrayInternalSAIS4M(b *testing.B) {
	benchmarkInternalSAIS(b, 4*1024*1024)
}

func BenchmarkSuffixArrayStdlibSAIS4M(b *testing.B) {
	benchmarkStdlibSAIS(b, 4*1024*1024)
}

func benchmarkPrefixDoubling(b *testing.B, size int) {
	data := benchmarkCorpus(size)
	symbols := make([]uint16, 0, len(data)+1)
	for _, value := range data {
		symbols = append(symbols, byteToSym(value))
	}
	symbols = append(symbols, terminalSym)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSuffixArrayResult = suffixArrayPrefixDoubling(symbols)
	}
}

func benchmarkInternalSAIS(b *testing.B, size int) {
	data := benchmarkCorpus(size)
	symbols := make([]uint16, 0, len(data)+1)
	for _, value := range data {
		symbols = append(symbols, byteToSym(value))
	}
	symbols = append(symbols, terminalSym)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var err error
		benchmarkSuffixArrayResult, err = suffixArray(symbols)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkStdlibSAIS(b *testing.B, size int) {
	data := benchmarkCorpus(size)
	data = append(data, 0)

	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkStdlibIndexResult = suffixarray.New(data)
	}
}

func benchmarkCorpus(size int) []byte {
	fragments := [][]byte{
		[]byte("<?php eval(base64_decode($payload)); ?>\n"),
		[]byte("function sort_users($arGn510VL9Ju075o) { return str_replace('x', 'y', $arGn510VL9Ju075o); }\n"),
		[]byte("<script>window.atob(token.replace(/-/g,'+'));</script>\n"),
		[]byte("public function updateConfig($input) { return base64_encode(str_replace('\\n', '', $input)); }\n"),
	}

	var data bytes.Buffer
	for data.Len() < size {
		for _, fragment := range fragments {
			if data.Len() >= size {
				break
			}
			remaining := size - data.Len()
			if len(fragment) > remaining {
				data.Write(fragment[:remaining])
				break
			}
			data.Write(fragment)
		}
	}
	return data.Bytes()
}
