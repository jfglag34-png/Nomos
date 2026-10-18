package core

import cf "github.com/wykserdex/wedra/internal/conformance"

type ConformanceCheck = cf.ConformanceCheck
type ConformanceReport = cf.ConformanceReport

func RunConformance(dir string) ConformanceReport { return cf.RunConformance(dir) }
