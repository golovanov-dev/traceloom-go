package traceloom

import "testing"

func BenchmarkTraceEvent(b *testing.B) {
	tracer, err := New(b.TempDir(), WithFailOnError(true), WithMaxFileBytes(1024*1024*1024))
	if err != nil {
		b.Fatal(err)
	}
	trace, err := tracer.Start("benchmark-trace")
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := trace.Event("benchmark", Data{"index": index, "value": "small payload"}); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := tracer.Close(); err != nil {
		b.Fatal(err)
	}
}
